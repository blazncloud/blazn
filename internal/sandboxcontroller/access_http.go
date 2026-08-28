package sandboxcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/sandbox"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

const (
	accessExecTimeout = 10 * time.Minute
	accessFileTimeout = 55 * time.Second
)

var accessGrantPath = regexp.MustCompile(`^/internal/v1/sandbox-access-grants/([0-9a-f-]{36})/(exec|file)$`)

type AccessGrantStore interface {
	ConsumeAccessGrant(context.Context, string, string, string) (AccessGrantBinding, bool, error)
}

type AccessExecutor interface {
	Execute(context.Context, AccessGrantBinding, string, []string, io.Reader) (AccessCommandResult, error)
}

type AccessHandler struct {
	store AccessGrantStore
	exec  AccessExecutor
}

func NewAccessHandler(store AccessGrantStore, executor AccessExecutor) (*AccessHandler, error) {
	if store == nil || executor == nil {
		return nil, errors.New("sandbox access dependencies are required")
	}
	return &AccessHandler{store: store, exec: executor}, nil
}

func (h *AccessHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	match := accessGrantPath.FindStringSubmatch(request.URL.Path)
	if match == nil || request.URL.RawQuery != "" {
		accessError(response, http.StatusNotFound, "not_found")
		return
	}
	kind := "exec"
	if match[2] == "file" {
		if request.Method == http.MethodPut {
			kind = "upload"
		} else if request.Method == http.MethodGet {
			kind = "download"
		} else {
			accessError(response, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
	} else if request.Method != http.MethodPost {
		accessError(response, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	token := strings.TrimPrefix(request.Header.Get("Authorization"), "Blazn-Grant ")
	if len(token) < 43 || len(token) > 256 || strings.ContainsAny(token, " \t\r\n") {
		accessError(response, http.StatusGone, "grant_invalid")
		return
	}
	var command []string
	var path, digest string
	var size int64
	if kind == "exec" {
		var payload struct {
			Command []string `json:"command"`
		}
		decoder := json.NewDecoder(io.LimitReader(request.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&payload) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(payload.Command) < 1 || len(payload.Command) > 32 {
			accessError(response, http.StatusBadRequest, "invalid_request")
			return
		}
		command = payload.Command
		for _, argument := range command {
			if argument == "" || len(argument) > 1024 {
				accessError(response, http.StatusBadRequest, "invalid_request")
				return
			}
		}
	} else {
		path = request.Header.Get("X-Blazn-Sandbox-Path")
		if !sandbox.ValidTransferPath(path) {
			accessError(response, http.StatusBadRequest, "invalid_request")
			return
		}
		if kind == "upload" {
			digest = request.Header.Get("X-Content-SHA256")
			size, _ = strconv.ParseInt(request.Header.Get("X-Content-Size"), 10, 64)
			if request.ContentLength != size || size < 0 || size > sandboxio.MaxAccessFileBytes || !sandbox.ValidDigest(digest) {
				accessError(response, http.StatusBadRequest, "invalid_request")
				return
			}
		}
	}
	hash := sha256.Sum256([]byte(token))
	binding, consumed, err := h.store.ConsumeAccessGrant(request.Context(), match[1], hex.EncodeToString(hash[:]), kind)
	if err != nil {
		accessError(response, http.StatusServiceUnavailable, "access_unavailable")
		return
	}
	if !consumed {
		accessError(response, http.StatusGone, "grant_invalid")
		return
	}
	timeout := accessFileTimeout
	if kind == "exec" {
		timeout = accessExecTimeout
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	switch kind {
	case "exec":
		result, err := h.exec.Execute(ctx, binding, "main", command, nil)
		if err != nil {
			log.Printf("sandbox access exec failed: %v", err)
			accessError(response, http.StatusBadGateway, "sandbox_exec_failed")
			return
		}
		accessJSON(response, map[string]any{"remoteExitCode": result.ExitCode, "stdoutBase64": base64.StdEncoding.EncodeToString(result.Stdout), "stderrBase64": base64.StdEncoding.EncodeToString(result.Stderr), "truncated": result.StdoutTruncated || result.StderrTruncated})
	case "upload":
		result, err := h.exec.Execute(ctx, binding, sandboxio.AccessContainer, []string{sandboxio.HelperBinary, "access-upload", path, strconv.FormatInt(size, 10), digest}, io.LimitReader(request.Body, sandboxio.MaxAccessFileBytes+1))
		if err != nil || result.ExitCode != 0 || len(result.Stdout) != 0 || len(result.Stderr) != 0 || result.StdoutTruncated || result.StderrTruncated {
			if err != nil {
				log.Printf("sandbox access upload failed: %v", err)
			}
			accessError(response, http.StatusBadGateway, "sandbox_upload_failed")
			return
		}
		accessJSON(response, map[string]any{"path": path, "size": size, "sha256": digest})
	case "download":
		result, err := h.exec.Execute(ctx, binding, sandboxio.AccessContainer, []string{sandboxio.HelperBinary, "access-download", path}, nil)
		if err != nil || result.ExitCode != 0 || len(result.Stderr) != 0 || result.StdoutTruncated || result.StderrTruncated {
			if err != nil {
				log.Printf("sandbox access download failed: %v", err)
			}
			accessError(response, http.StatusBadGateway, "sandbox_download_failed")
			return
		}
		digestBytes := sha256.Sum256(result.Stdout)
		response.Header().Set("Content-Type", "application/octet-stream")
		response.Header().Set("X-Content-Size", strconv.Itoa(len(result.Stdout)))
		response.Header().Set("X-Content-SHA256", "sha256:"+hex.EncodeToString(digestBytes[:]))
		response.Header().Set("Content-Length", strconv.Itoa(len(result.Stdout)))
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write(result.Stdout)
	}
}

func accessJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(value)
}

func accessError(response http.ResponseWriter, status int, code string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_, _ = fmt.Fprintf(response, `{"code":%q,"message":%q}`, code, code)
}
