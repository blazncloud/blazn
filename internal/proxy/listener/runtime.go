// Package listener owns the bounded, activation-local HTTP server around the
// frozen proxy router. Durable activation and CLI publication live elsewhere.
package listener

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/router"
)

const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 30 * time.Second
	defaultIdleTimeout       = 30 * time.Second
	defaultMaxHeaderBytes    = 32 << 10
)

type Config struct {
	Address           string
	Port              uint16
	Credential        Credential
	Router            router.Config
	PreflightTimeout  time.Duration
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}

type Runtime struct {
	listener   net.Listener
	server     *http.Server
	done       chan struct{}
	errMu      sync.Mutex
	err        error
	credential Credential
}

func Start(config Config) (*Runtime, error) {
	address, err := netip.ParseAddr(config.Address)
	if err != nil || !address.IsLoopback() {
		return nil, errors.New("listener address must be an explicit loopback IP")
	}
	credential := config.Credential
	if credential.authenticateValue() == "" {
		credential, err = GenerateCredential()
		if err != nil {
			return nil, err
		}
	} else if _, err := ParseCredential(credential.authenticateValue()); err != nil {
		return nil, err
	}
	config.Router.ListenerToken = credential.authenticateValue()
	// The POC wire contract permits destination credentials only as Bearer.
	// Runtime callers may inject resolution, but not a different wire adapter.
	config.Router.CredentialApply = router.BearerCredentialAdapter{}
	handler, err := router.NewHandler(config.Router)
	if err != nil {
		return nil, fmt.Errorf("create proxy router: %w", err)
	}
	preflightTimeout := config.PreflightTimeout
	if preflightTimeout <= 0 {
		preflightTimeout = 30 * time.Second
	}
	preflightCtx, preflightCancel := context.WithTimeout(context.Background(), preflightTimeout)
	err = handler.Preflight(preflightCtx)
	preflightCancel()
	if err != nil {
		return nil, fmt.Errorf("proxy preflight: %w", err)
	}
	readHeaderTimeout := config.ReadHeaderTimeout
	if readHeaderTimeout <= 0 {
		readHeaderTimeout = defaultReadHeaderTimeout
	}
	readTimeout := config.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = defaultReadTimeout
	}
	idleTimeout := config.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultIdleTimeout
	}
	maxHeaderBytes := config.MaxHeaderBytes
	if maxHeaderBytes <= 0 {
		maxHeaderBytes = defaultMaxHeaderBytes
	}
	bind := net.JoinHostPort(address.String(), strconv.Itoa(int(config.Port)))
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("bind loopback listener: %w", err)
	}
	bound, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil || !bound.Addr().Unmap().IsLoopback() {
		_ = ln.Close()
		return nil, errors.New("listener did not bind to loopback")
	}
	runtime := &Runtime{listener: ln, done: make(chan struct{}), credential: credential}
	runtime.server = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() {
		err := runtime.server.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runtime.errMu.Lock()
			runtime.err = err
			runtime.errMu.Unlock()
		}
		close(runtime.done)
	}()
	return runtime, nil
}

func (r *Runtime) Address() string       { return r.listener.Addr().String() }
func (r *Runtime) Done() <-chan struct{} { return r.done }
func (r *Runtime) ChildEnvironment(base []string) ([]string, error) {
	environment, err := r.credential.ChildEnvironment(base)
	if err != nil {
		return nil, err
	}
	filtered := environment[:0]
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if name != "OPENAI_BASE_URL" && name != "ANTHROPIC_BASE_URL" {
			filtered = append(filtered, entry)
		}
	}
	baseURL := (&url.URL{Scheme: "http", Host: r.Address()}).String()
	return append(filtered, "OPENAI_BASE_URL="+baseURL+"/v1", "ANTHROPIC_BASE_URL="+baseURL), nil
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	err := r.server.Shutdown(ctx)
	select {
	case <-r.done:
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
	r.errMu.Lock()
	serveErr := r.err
	r.errMu.Unlock()
	return errors.Join(err, serveErr)
}
