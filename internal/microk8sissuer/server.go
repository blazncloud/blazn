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
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type peerKey struct{}
type Peer struct{ UID, GID uint32 }
type Server struct {
	Service                *Service
	AllowedUID, AllowedGID uint32
	SocketUID              uint32
	Timeout                time.Duration
	// HealthCache, when positive, serves a successful readiness result for up
	// to that age and refreshes it in the background once a quarter of it has
	// elapsed. MicroK8s status can take several seconds on a loaded control
	// plane, which exceeds the broker's short health deadline. Credential
	// issuance never uses this cache; it runs its own readiness probe.
	HealthCache time.Duration

	healthMu         sync.Mutex
	healthOK         time.Time
	healthRefreshing bool
	coldProbe        *healthProbe
	now              func() time.Time
}

// healthProbe is one detached readiness probe shared by every caller that
// arrives while the cache is cold.
type healthProbe struct {
	done chan struct{}
	err  error
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Server) probeHealth(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, s.Timeout)
	defer cancel()
	err := s.Service.Health(ctx)
	if err == nil {
		s.healthMu.Lock()
		s.healthOK = s.clock()
		s.healthMu.Unlock()
	}
	return err
}

func (s *Server) healthy(ctx context.Context) error {
	if s.HealthCache <= 0 {
		return s.probeHealth(ctx)
	}
	s.healthMu.Lock()
	age := s.clock().Sub(s.healthOK)
	cached := !s.healthOK.IsZero() && age >= 0 && age < s.HealthCache
	if cached && age >= s.HealthCache/4 && !s.healthRefreshing {
		s.healthRefreshing = true
		go func() {
			_ = s.probeHealth(context.Background())
			s.healthMu.Lock()
			s.healthRefreshing = false
			s.healthMu.Unlock()
		}()
	}
	if cached {
		s.healthMu.Unlock()
		return nil
	}
	// A cold probe must not run on the caller's context: the broker gives up
	// after about two seconds, a loaded MicroK8s status takes longer, and a
	// probe cancelled with its caller never warms the cache, so readiness
	// would fail forever. The probe runs detached under the server timeout;
	// a caller that gives up still leaves a warm cache for the next one.
	probe := s.coldProbe
	if probe == nil {
		probe = &healthProbe{done: make(chan struct{})}
		s.coldProbe = probe
		go func() {
			err := s.probeHealth(context.Background())
			s.healthMu.Lock()
			probe.err = err
			s.coldProbe = nil
			s.healthMu.Unlock()
			close(probe.done)
		}()
	}
	s.healthMu.Unlock()
	select {
	case <-probe.done:
		return probe.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) Serve(socketPath string) error {
	if s.Service == nil || s.Timeout < time.Second || s.Timeout > 30*time.Second {
		return fmt.Errorf("server configuration is invalid")
	}
	if err := s.prepareSocketPath(socketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(socketPath, 0660); err != nil {
		return err
	}
	if err = os.Chown(socketPath, int(s.SocketUID), int(s.AllowedGID)); err != nil {
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

func (s *Server) prepareSocketPath(socketPath string) error {
	parent := filepath.Dir(socketPath)
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0750 || stat.Uid != s.SocketUID || stat.Gid != s.AllowedGID {
		return fmt.Errorf("issuer socket directory is unsafe")
	}
	before, err := os.Lstat(socketPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	beforeStat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || before.Mode()&os.ModeSocket == 0 || before.Mode().Perm() != 0660 || beforeStat.Uid != s.SocketUID || beforeStat.Gid != s.AllowedGID || beforeStat.Nlink != 1 {
		return fmt.Errorf("issuer socket path is unsafe")
	}
	connection, dialErr := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if dialErr == nil {
		connection.Close()
		return fmt.Errorf("issuer socket is already active")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return fmt.Errorf("issuer socket state is ambiguous")
	}
	after, err := os.Lstat(socketPath)
	if err != nil {
		return err
	}
	afterStat := after.Sys().(*syscall.Stat_t)
	if afterStat.Dev != beforeStat.Dev || afterStat.Ino != beforeStat.Ino || after.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("issuer socket changed during stale recovery")
	}
	if err := os.Remove(socketPath); err != nil {
		return err
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")
	peer, ok := r.Context().Value(peerKey{}).(Peer)
	if !ok || (peer.UID != s.AllowedUID && peer.GID != s.AllowedGID) {
		s.writeError(w, http.StatusForbidden, &ProtocolError{Code: "peer_denied", Message: "socket peer is not authorized"})
		return
	}
	if r.Method == "GET" && r.URL.Path == "/healthz" && r.URL.RawQuery == "" {
		if err := s.healthy(r.Context()); err != nil {
			s.writeError(w, http.StatusServiceUnavailable, &ProtocolError{Code: "microk8s_unavailable", Message: "MicroK8s readiness check failed"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": SchemaVersion, "operation": "health", "healthy": true})
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
