//go:build linux

package microk8sissuer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

type peerKey struct{}
type Peer struct{ UID, GID uint32 }
type Server struct {
	Service                *Service
	AllowedUID, AllowedGID uint32
	Timeout                time.Duration
}

func (s *Server) Serve(socketPath string) error {
	if s.Service == nil || s.Timeout < time.Second || s.Timeout > 30*time.Second {
		return fmt.Errorf("server configuration is invalid")
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		return fmt.Errorf("socket path already exists")
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(socketPath, 0660); err != nil {
		return err
	}
	if err = os.Chown(socketPath, 0, int(s.AllowedGID)); err != nil {
		return err
	}
	server := &http.Server{ReadHeaderTimeout: s.Timeout, ReadTimeout: s.Timeout, WriteTimeout: s.Timeout, IdleTimeout: s.Timeout, MaxHeaderBytes: 4096, ConnContext: func(ctx context.Context, c net.Conn) context.Context {
		peer, err := peerCredential(c)
		if err != nil {
			return ctx
		}
		return context.WithValue(ctx, peerKey{}, peer)
	}, Handler: http.HandlerFunc(s.handle)}
	return server.Serve(listener)
}
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")
	peer, ok := r.Context().Value(peerKey{}).(Peer)
	if !ok || (peer.UID != s.AllowedUID && peer.GID != s.AllowedGID) {
		s.writeError(w, http.StatusForbidden, &ProtocolError{Code: "peer_denied", Message: "socket peer is not authorized"})
		return
	}
	if r.Method != "POST" || r.URL.Path != "/v1/worker-credentials" || r.URL.RawQuery != "" || r.Header.Get("content-type") != "application/json" {
		s.writeError(w, http.StatusBadRequest, invalid("HTTP request is invalid"))
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxMessageBytes+1))
	if err != nil || len(data) > MaxMessageBytes {
		s.writeError(w, http.StatusBadRequest, invalid("request size is invalid"))
		return
	}
	req, err := DecodeRequest(data)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.Timeout)
	defer cancel()
	result, err := s.Service.Handle(ctx, req)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, context.DeadlineExceeded) {
			err = &ProtocolError{Code: "deadline_exceeded", Message: "issuer deadline exceeded"}
			status = http.StatusGatewayTimeout
		}
		s.writeError(w, status, err)
		return
	}
	json.NewEncoder(w).Encode(result)
}
func (s *Server) writeError(w http.ResponseWriter, status int, err error) {
	pe := asProtocol(err)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"schemaVersion": SchemaVersion, "operation": "error", "code": pe.Code, "message": pe.Message})
}
func peerCredential(c net.Conn) (Peer, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return Peer{}, fmt.Errorf("not unix")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var cred *syscall.Ucred
	var opErr error
	err = raw.Control(func(fd uintptr) {
		cred, opErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return Peer{}, err
	}
	if opErr != nil {
		return Peer{}, opErr
	}
	return Peer{UID: cred.Uid, GID: cred.Gid}, nil
}
