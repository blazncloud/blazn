//go:build darwin || linux

package process

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/listener"
	"github.com/blazncloud/blazn/internal/proxy/router"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

// ResolvedRuntimeFactory starts a listener solely from the policy and
// destination credentials already carried by the authenticated bootstrap. It
// never resolves or persists credentials and is intentionally not installed as
// DefaultChildMain's factory until the durable activation owner is merged.
type ResolvedRuntimeFactory struct {
	Platform         *UnixPlatform
	ControlDirectory string
	ControlTimeout   time.Duration
}

func (f ResolvedRuntimeFactory) Start(ctx context.Context, bootstrap Bootstrap) (Runtime, error) {
	if err := validateBootstrap(bootstrap); err != nil || f.Platform == nil {
		return nil, ErrUnavailable
	}
	if err := validateControlDirectory(f.ControlDirectory, bootstrap.Metadata.OwnerUID); err != nil {
		return nil, err
	}
	evidence, live, err := f.Platform.Evidence(ctx, os.Getpid())
	if err != nil || !live || evidence.OwnerUID != bootstrap.Metadata.OwnerUID || evidence.ExecutablePath != bootstrap.Metadata.BinaryPath || evidence.BinaryDigest != bootstrap.Metadata.BinaryDigest {
		return nil, ErrUnauthorized
	}
	policy, err := proxycontract.DecodePolicy(bytes.NewReader(bootstrap.Policy))
	if err != nil {
		return nil, ErrProtocol
	}
	credentials := &resolvedCredentials{values: make(map[string][]byte, len(bootstrap.Credentials))}
	for _, item := range bootstrap.Credentials {
		if _, exists := credentials.values[item.Reference]; exists {
			credentials.zero()
			return nil, ErrProtocol
		}
		credentials.values[item.Reference] = append([]byte(nil), item.Value...)
	}
	proxyRuntime, err := listener.Start(listener.Config{Address: "127.0.0.1", Router: router.Config{
		Policy:       policy,
		ActivationID: bootstrap.Metadata.ActivationID,
		Credentials:  credentials,
	}})
	if err != nil {
		credentials.zero()
		return nil, safeError(err)
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), DefaultControlTTL)
		defer cancel()
		_ = proxyRuntime.Shutdown(cleanupCtx)
		credentials.zero()
	}
	token, err := freshChallenge()
	if err != nil {
		cleanup()
		return nil, ErrUnavailable
	}
	controlAddress := filepath.Join(f.ControlDirectory, "proxy-"+token[:16]+".sock")
	control, err := net.ListenUnix("unix", &net.UnixAddr{Name: controlAddress, Net: "unix"})
	if err != nil {
		cleanup()
		return nil, safeError(err)
	}
	control.SetUnlinkOnClose(true)
	if err := os.Chmod(controlAddress, 0600); err != nil {
		_ = control.Close()
		cleanup()
		return nil, ErrUnavailable
	}
	info, err := os.Lstat(controlAddress)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || statUID(info) != bootstrap.Metadata.OwnerUID {
		_ = control.Close()
		cleanup()
		return nil, ErrUnauthorized
	}
	timeout := f.ControlTimeout
	if timeout <= 0 {
		timeout = DefaultControlTTL
	}
	return &resolvedRuntime{
		proxy:       proxyRuntime,
		credentials: credentials,
		control:     control,
		controlPath: controlAddress,
		token:       token,
		evidence:    evidence,
		timeout:     timeout,
		done:        make(chan struct{}),
	}, nil
}

func validateControlDirectory(path string, uid int) error {
	if uid < 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrUnauthorized
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || statUID(info) != uid {
		return ErrUnauthorized
	}
	return nil
}

type resolvedCredentials struct {
	mu     sync.RWMutex
	values map[string][]byte
}

func (c *resolvedCredentials) DestinationCredential(_ context.Context, reference string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value, ok := c.values[reference]
	if !ok || len(value) == 0 {
		return "", router.ErrCredentialUnavailable
	}
	return string(value), nil
}

func (c *resolvedCredentials) zero() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for reference, value := range c.values {
		zeroBytes(value)
		delete(c.values, reference)
	}
}

type resolvedRuntime struct {
	proxy       *listener.Runtime
	credentials *resolvedCredentials
	control     *net.UnixListener
	controlPath string
	token       string
	evidence    Evidence
	timeout     time.Duration
	shutdown    sync.Once
	done        chan struct{}
	shutdownErr error
	connections sync.WaitGroup
}

func (r *resolvedRuntime) Address() string        { return r.proxy.Address() }
func (r *resolvedRuntime) ControlAddress() string { return r.controlPath }
func (r *resolvedRuntime) ListenerToken() string  { return r.token }
func (r *resolvedRuntime) Identity() (int, string, string) {
	return r.evidence.PID, r.evidence.ProcessStartIdentity, r.evidence.ExecutableIdentity
}

func (r *resolvedRuntime) ServeControl(ctx context.Context, handler func(context.Context, ControlRequest) (ControlResponse, error)) error {
	if r == nil || r.control == nil || handler == nil {
		return ErrUnavailable
	}
	cancelled := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = r.control.Close()
		case <-cancelled:
		}
	}()
	defer close(cancelled)
	for {
		connection, err := r.control.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				r.connections.Wait()
				return ctx.Err()
			}
			return safeError(err)
		}
		r.connections.Add(1)
		go func(connection *net.UnixConn) {
			defer r.connections.Done()
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(r.timeout))
			var request ControlRequest
			if err := readFrame(connection, MaxControlBytes, &request); err != nil {
				return
			}
			response, err := handler(ctx, request)
			if err != nil {
				return
			}
			_ = writeFrame(connection, MaxControlBytes, response)
		}(connection)
	}
}

func (r *resolvedRuntime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.shutdown.Do(func() {
		_ = r.control.Close()
		r.shutdownErr = r.proxy.Shutdown(ctx)
		r.credentials.zero()
		close(r.done)
	})
	select {
	case <-r.done:
		return r.shutdownErr
	case <-ctx.Done():
		return errors.Join(r.shutdownErr, ctx.Err())
	}
}
