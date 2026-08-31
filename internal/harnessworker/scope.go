package harnessworker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

type WorkloadScopeValidator interface {
	ValidateWorkloadScope(context.Context, WorkloadScope) error
}

type StaticScopeValidator struct{ Expected WorkloadScope }

func (v StaticScopeValidator) ValidateWorkloadScope(ctx context.Context, actual WorkloadScope) error {
	if err := ctx.Err(); err != nil {
		return protocolError("scope_validation_cancelled")
	}
	if err := v.Expected.ValidateAt(time.Now()); err != nil || actual != v.Expected {
		return protocolError("workload_scope_mismatch")
	}
	return nil
}

type FileScopeValidator struct{ Path string }

func (v FileScopeValidator) ValidateWorkloadScope(ctx context.Context, actual WorkloadScope) error {
	if err := ctx.Err(); err != nil {
		return protocolError("scope_validation_cancelled")
	}
	file, info, err := openProtectedFile(v.Path, MaxProtocolLineBytes)
	if err != nil {
		return protocolError("workload_scope_unavailable")
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, MaxProtocolLineBytes+1))
	if err != nil || len(body) == 0 || len(body) > MaxProtocolLineBytes {
		return protocolError("workload_scope_unavailable")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(info, after) || !protectedInfo(after, int64(len(body))) {
		return protocolError("workload_scope_unavailable")
	}
	expected, err := DecodeWorkloadScope(bytes.NewReader(body), time.Now())
	if err != nil || expected != actual {
		return protocolError("workload_scope_mismatch")
	}
	return nil
}

type ListenerTokenSource interface {
	OpenListenerToken(context.Context, string) (*os.File, error)
}

type ProtectedListenerTokenFile struct{ Path string }

func (s ProtectedListenerTokenFile) OpenListenerToken(ctx context.Context, fingerprint string) (*os.File, error) {
	if err := ctx.Err(); err != nil || !digestPattern.MatchString(fingerprint) {
		return nil, protocolError("listener_token_unavailable")
	}
	file, before, err := openProtectedFile(s.Path, MaxListenerTokenBytes)
	if err != nil {
		return nil, protocolError("listener_token_unavailable")
	}
	closeWithError := func() (*os.File, error) {
		_ = file.Close()
		return nil, protocolError("listener_token_unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(file, MaxListenerTokenBytes+1))
	if err != nil || len(body) == 0 || len(body) > MaxListenerTokenBytes {
		return closeWithError()
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !protectedInfo(after, int64(len(body))) || digestBytes(body) != fingerprint {
		return closeWithError()
	}
	for index := range body {
		body[index] = 0
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return closeWithError()
	}
	return file, nil
}

func openProtectedFile(name string, maxBytes int64) (*os.File, os.FileInfo, error) {
	if name == "" || !filepath.IsAbs(name) || filepath.Clean(name) != name || maxBytes < 1 {
		return nil, nil, errors.New("protected file path is invalid")
	}
	before, err := os.Lstat(name)
	if err != nil || !protectedInfo(before, maxBytes) {
		return nil, nil, errors.New("protected file metadata is invalid")
	}
	file, err := openNoFollow(name)
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !protectedInfo(after, maxBytes) {
		_ = file.Close()
		return nil, nil, errors.New("protected file changed")
	}
	return file, after, nil
}
