package sandboxcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
	"golang.org/x/net/websocket"
)

const maxAccessStreamBytes = sandboxio.MaxAccessFileBytes

type KubernetesAccessRuntime struct {
	client    *http.Client
	transport *kubernetesExecTransport
	baseURL   string
}

type AccessCommandResult struct {
	ExitCode                         int
	Stdout, Stderr                   []byte
	StdoutTruncated, StderrTruncated bool
}

func NewKubernetesAccessRuntime(config KubernetesConfig) (*KubernetesAccessRuntime, error) {
	client, err := newKubernetesHTTPClient(config)
	if err != nil {
		return nil, err
	}
	transport, err := newKubernetesExecTransport(config)
	if err != nil {
		return nil, err
	}
	return &KubernetesAccessRuntime{client: client, transport: transport, baseURL: strings.TrimSuffix(config.BaseURL, "/")}, nil
}

func (r *KubernetesAccessRuntime) Execute(ctx context.Context, binding AccessGrantBinding, container string, command []string, input io.Reader) (AccessCommandResult, error) {
	if r == nil || r.client == nil || r.transport == nil || binding.SandboxID == "" || binding.WorkspaceID == "" || binding.RequestedBy == "" || binding.BackendUID == "" || container != "main" && container != sandboxio.AccessContainer || len(command) == 0 || len(command) > 32 {
		return AccessCommandResult{}, errors.New("sandbox access execution binding is invalid")
	}
	for _, argument := range command {
		if argument == "" || len(argument) > 1024 {
			return AccessCommandResult{}, errors.New("sandbox access command is invalid")
		}
	}
	target, err := r.target(ctx, binding, container)
	if err != nil {
		return AccessCommandResult{}, err
	}
	return r.transport.executeAccess(ctx, target, command, input)
}

func (r *KubernetesAccessRuntime) target(ctx context.Context, binding AccessGrantBinding, container string) (sandboxio.FrozenPodTarget, error) {
	endpoint := r.baseURL + "/api/v1/namespaces/" + sandboxcontrol.Namespace + "/pods/" + url.PathEscape(binding.SandboxID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return sandboxio.FrozenPodTarget{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "blazn-sandbox-access/v1")
	response, err := r.client.Do(request)
	if err != nil {
		return sandboxio.FrozenPodTarget{}, errors.New("sandbox access Pod observation failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return sandboxio.FrozenPodTarget{}, fmt.Errorf("sandbox access Pod observation returned HTTP %d", response.StatusCode)
	}
	var pod struct {
		Metadata struct {
			Name, Namespace, UID string
			Labels               map[string]string
			OwnerReferences      []struct {
				APIVersion, Kind, Name, UID string
				Controller                  *bool
			}
		}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxKubernetesResponseBytes+1))
	if err := decoder.Decode(&pod); err != nil || decoder.Decode(&struct{}{}) != io.EOF || pod.Metadata.Name != binding.SandboxID || pod.Metadata.Namespace != sandboxcontrol.Namespace || pod.Metadata.UID == "" || pod.Metadata.Labels[sandboxcontrol.ManagedLabel] != "true" || pod.Metadata.Labels[sandboxcontrol.WorkspaceLabel] != binding.WorkspaceID || pod.Metadata.Labels[sandboxcontrol.OwnerLabel] != binding.RequestedBy || pod.Metadata.Labels[sandboxcontrol.SandboxIDLabel] != binding.SandboxID || !ownedBySandbox(pod.Metadata.OwnerReferences, binding.SandboxID, binding.BackendUID) {
		return sandboxio.FrozenPodTarget{}, errors.New("sandbox access Pod identity is invalid")
	}
	return sandboxio.FrozenPodTarget{Namespace: sandboxcontrol.Namespace, PodName: pod.Metadata.Name, PodUID: pod.Metadata.UID, SandboxUID: binding.BackendUID, Container: container}, nil
}

func ownedBySandbox(references []struct {
	APIVersion, Kind, Name, UID string
	Controller                  *bool
}, name, uid string) bool {
	for _, reference := range references {
		if reference.APIVersion == sandboxcontrol.APIVersion && reference.Kind == sandboxcontrol.Kind && reference.Name == name && reference.UID == uid && reference.Controller != nil && *reference.Controller {
			return true
		}
	}
	return false
}

func (t *kubernetesExecTransport) executeAccess(ctx context.Context, target sandboxio.FrozenPodTarget, command []string, input io.Reader) (AccessCommandResult, error) {
	token, err := t.readToken()
	if err != nil {
		return AccessCommandResult{}, err
	}
	values := url.Values{"container": {target.Container}, "stdin": {"true"}, "stdout": {"true"}, "stderr": {"true"}, "tty": {"false"}}
	for _, argument := range command {
		values.Add("command", argument)
	}
	location := strings.Replace(t.baseURL, "https://", "wss://", 1) + "/api/v1/namespaces/" + url.PathEscape(target.Namespace) + "/pods/" + url.PathEscape(target.PodName) + "/exec?" + values.Encode()
	configuration, err := websocket.NewConfig(location, t.baseURL)
	if err != nil {
		return AccessCommandResult{}, errors.New("sandbox access WebSocket configuration is invalid")
	}
	configuration.Protocol = []string{kubernetesExecProtocol}
	configuration.TlsConfig = t.tlsConfig.Clone()
	configuration.Dialer = t.dialer
	configuration.Header = http.Header{"Authorization": {"Bearer " + token}, "User-Agent": {"blazn-sandbox-access-exec/v1"}}
	connection, err := configuration.DialContext(ctx)
	if err != nil {
		return AccessCommandResult{}, errors.New("sandbox access WebSocket handshake failed")
	}
	defer connection.Close()
	if connection.Config() == nil || len(connection.Config().Protocol) != 1 || connection.Config().Protocol[0] != kubernetesExecProtocol {
		return AccessCommandResult{}, errors.New("sandbox access did not negotiate v5.channel.k8s.io")
	}
	deadline := time.Now().Add(55 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return AccessCommandResult{}, err
	}
	if input != nil {
		buffer := make([]byte, 32<<10)
		for {
			count, readErr := input.Read(buffer)
			if count > 0 {
				if err := websocket.Message.Send(connection, append([]byte{streamStdin}, buffer[:count]...)); err != nil {
					return AccessCommandResult{}, errors.New("sandbox access stdin write failed")
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return AccessCommandResult{}, errors.New("sandbox access stdin read failed")
			}
		}
	}
	if err := websocket.Message.Send(connection, []byte{streamClose, streamStdin}); err != nil {
		return AccessCommandResult{}, errors.New("sandbox access stdin close failed")
	}
	result := AccessCommandResult{}
	var stdout, stderr bytes.Buffer
	for {
		var message []byte
		if err := websocket.Message.Receive(connection, &message); err != nil || len(message) == 0 {
			return AccessCommandResult{}, errors.New("sandbox access ended without a status frame")
		}
		switch message[0] {
		case streamStdout:
			appendAccessOutput(&stdout, message[1:], &result.StdoutTruncated)
		case streamStderr:
			appendAccessOutput(&stderr, message[1:], &result.StderrTruncated)
		case streamError:
			code, statusErr := accessExitStatus(message[1:])
			if statusErr != nil {
				return AccessCommandResult{}, statusErr
			}
			result.ExitCode, result.Stdout, result.Stderr = code, stdout.Bytes(), stderr.Bytes()
			return result, nil
		case streamClose:
			if len(message) != 2 || message[1] > streamError {
				return AccessCommandResult{}, errors.New("sandbox access close frame is invalid")
			}
		default:
			return AccessCommandResult{}, errors.New("sandbox access returned an unknown stream")
		}
	}
}

func appendAccessOutput(output *bytes.Buffer, body []byte, truncated *bool) {
	remaining := maxAccessStreamBytes - output.Len()
	if remaining <= 0 {
		*truncated = true
		return
	}
	if len(body) > remaining {
		body = body[:remaining]
		*truncated = true
	}
	_, _ = output.Write(body)
}

func accessExitStatus(body []byte) (int, error) {
	var status struct {
		Status  string `json:"status"`
		Details struct {
			Causes []struct{ Reason, Message string } `json:"causes"`
		} `json:"details"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&status); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return 0, errors.New("sandbox access status frame is invalid")
	}
	if status.Status == "Success" {
		return 0, nil
	}
	if status.Status == "Failure" {
		for _, cause := range status.Details.Causes {
			if cause.Reason == "ExitCode" {
				code, err := strconv.Atoi(cause.Message)
				if err == nil && code >= 0 && code <= 255 {
					return code, nil
				}
			}
		}
	}
	return 0, errors.New("sandbox access process status is invalid")
}
