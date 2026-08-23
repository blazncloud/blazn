package process

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/state"
)

const (
	testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testNonce  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

func testMetadata() Metadata {
	return Metadata{ActivationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Nonce: testNonce, Generation: 2, OwnerUID: 1000, Mode: "session", SessionIdentity: "uid:1000/session:test", BinaryPath: "/usr/local/bin/blazn", BinaryDigest: testDigest}
}

func testStartRequest() StartRequest {
	return StartRequest{Metadata: testMetadata(), Policy: []byte(`{"schemaVersion":"proxy/v1alpha1"}`), Credentials: []Credential{{Reference: "workspace-vault://provider", Value: []byte("destination-secret")}}}
}

func TestProtocolRejectsPartialOversizeUnknownAndStalledFrames(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		var output Bootstrap
		if err := readFrame(bytes.NewReader([]byte{0, 0, 0, 8, '{'}), MaxBootstrapBytes, &output); !errors.Is(err, ErrProtocol) {
			t.Fatalf("expected protocol error, got %v", err)
		}
	})
	t.Run("oversize", func(t *testing.T) {
		var frame [4]byte
		binary.BigEndian.PutUint32(frame[:], MaxHandshakeBytes+1)
		var output Handshake
		if err := readFrame(bytes.NewReader(frame[:]), MaxHandshakeBytes, &output); !errors.Is(err, ErrProtocol) {
			t.Fatalf("expected protocol error, got %v", err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		payload := []byte(`{"version":"proxy-process/v1","kind":"bootstrap","unexpected":true}`)
		var frame bytes.Buffer
		_ = binary.Write(&frame, binary.BigEndian, uint32(len(payload)))
		frame.Write(payload)
		var output Bootstrap
		if err := readFrame(&frame, MaxBootstrapBytes, &output); !errors.Is(err, ErrProtocol) {
			t.Fatalf("expected closed-message rejection, got %v", err)
		}
	})
	t.Run("stalled", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer writer.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		var output Handshake
		if err := readFrameContext(ctx, reader, MaxHandshakeBytes, &output); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected deadline, got %v", err)
		}
	})
}

func TestControllerEndToEndFreshProofAndAuthenticatedStop(t *testing.T) {
	platform := newFakePlatform()
	controller := &Controller{Platform: platform, HandshakeTimeout: time.Second, ControlTimeout: time.Second, StopGrace: 100 * time.Millisecond}
	managed, err := controller.Start(context.Background(), testStartRequest())
	if err != nil {
		t.Fatal(err)
	}
	if managed.Identity().PID != platform.evidence.PID || managed.ListenerToken() != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatal("start did not return child identity and in-memory listener token")
	}
	proof, live, err := controller.Inspect(context.Background(), managed.Identity().PID)
	if err != nil || !live || proof != managed.Proof() {
		t.Fatalf("inspect proof mismatch: live=%v err=%v", live, err)
	}
	firstChallenge := platform.lastChallenge
	if _, _, err := controller.Inspect(context.Background(), managed.Identity().PID); err != nil || firstChallenge == platform.lastChallenge {
		t.Fatal("inspect did not use a fresh challenge")
	}
	if err := controller.Stop(context.Background(), managed.Proof()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-platform.runtime.stopped:
	case <-time.After(time.Second):
		t.Fatal("authenticated stop was not delivered")
	}
}

func TestControllerRejectsSubstitutedProcessEvidenceAndExpectedProof(t *testing.T) {
	mutations := map[string]func(*Evidence){
		"uid":   func(e *Evidence) { e.OwnerUID++ },
		"pid":   func(e *Evidence) { e.PID++ },
		"start": func(e *Evidence) { e.ProcessStartIdentity = "reused" },
		"path":  func(e *Evidence) { e.ExecutablePath = "/tmp/substitute" },
		"inode": func(e *Evidence) { e.ExecutableIdentity = "dev:2/inode:9" },
		"binary": func(e *Evidence) {
			e.BinaryDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			platform := newFakePlatform()
			controller := &Controller{Platform: platform, HandshakeTimeout: time.Second, ControlTimeout: time.Second}
			managed, err := controller.Start(context.Background(), testStartRequest())
			if err != nil {
				t.Fatal(err)
			}
			mutate(&platform.evidence)
			if _, live, err := controller.Inspect(context.Background(), managed.Identity().PID); err == nil && live {
				t.Fatal("substituted evidence was accepted")
			}
			if err := controller.Stop(context.Background(), managed.Proof()); err == nil {
				t.Fatal("stop authorized substituted process")
			}
		})
	}

	platform := newFakePlatform()
	controller := &Controller{Platform: platform, HandshakeTimeout: time.Second, ControlTimeout: time.Second}
	managed, err := controller.Start(context.Background(), testStartRequest())
	if err != nil {
		t.Fatal(err)
	}
	wrong := managed.Proof()
	wrong.Generation++
	if err := controller.Stop(context.Background(), wrong); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized stop returned %v", err)
	}
}

func TestControllerRejectsReplaySubstitutedKeySocketAndProof(t *testing.T) {
	for _, mode := range []string{"replay", "key", "proof", "socket"} {
		t.Run(mode, func(t *testing.T) {
			platform := newFakePlatform()
			controller := &Controller{Platform: platform, HandshakeTimeout: time.Second, ControlTimeout: 50 * time.Millisecond}
			managed, err := controller.Start(context.Background(), testStartRequest())
			if err != nil {
				t.Fatal(err)
			}
			platform.attack = mode
			if _, live, err := controller.Inspect(context.Background(), managed.Identity().PID); err == nil || live {
				t.Fatalf("%s substitution was accepted", mode)
			}
		})
	}
}

func TestStartCleansUpDeadParentChildAndBoundedTermKill(t *testing.T) {
	platform := newFakePlatform()
	platform.stallHandshake = true
	controller := &Controller{Platform: platform, HandshakeTimeout: 10 * time.Millisecond, StopGrace: 40 * time.Millisecond}
	if _, err := controller.Start(context.Background(), testStartRequest()); err == nil {
		t.Fatal("stalled handshake succeeded")
	}
	if platform.child.terminated != 1 || platform.child.killed != 1 {
		t.Fatalf("cleanup signals terminate=%d kill=%d", platform.child.terminated, platform.child.killed)
	}

	platform = newFakePlatform()
	platform.childDead = true
	controller = &Controller{Platform: platform, HandshakeTimeout: 50 * time.Millisecond, StopGrace: 20 * time.Millisecond}
	if _, err := controller.Start(context.Background(), testStartRequest()); err == nil {
		t.Fatal("dead child start succeeded")
	}
}

func TestBootstrapSecretsStayOffArgvEnvironmentFilesAndFormatting(t *testing.T) {
	platform := newFakePlatform()
	controller := &Controller{Platform: platform, HandshakeTimeout: time.Second}
	request := testStartRequest()
	secret := string(request.Credentials[0].Value)
	managed, err := controller.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(platform.spawn.Argv, " ") + platform.spawn.Executable + managed.String()
	if strings.Contains(joined, secret) || strings.Contains(joined, request.Credentials[0].Reference) {
		t.Fatal("bootstrap secret/reference escaped into process metadata or formatting")
	}
	if platform.environmentTouched || platform.filesTouched || strings.Contains(platform.logs.String(), secret) {
		t.Fatal("bootstrap material escaped anonymous pipes")
	}
	if got := (&Bootstrap{Credentials: request.Credentials}).String(); strings.Contains(got, secret) {
		t.Fatal("bootstrap formatting exposed secret")
	}
}

func TestHiddenInvocationIsExactAndDefaultFactoryUnavailable(t *testing.T) {
	if !IsChildInvocation([]string{ChildCommand, ProtocolVersion}) || IsChildInvocation([]string{ChildCommand}) || IsChildInvocation([]string{ChildCommand, ProtocolVersion, "secret"}) {
		t.Fatal("hidden child invocation was not exact")
	}
	reader, writer := io.Pipe()
	result := make(chan error, 1)
	go func() { result <- DefaultChildMain(context.Background(), reader, nopWriteCloser{io.Discard}) }()
	bootstrap := Bootstrap{Version: ProtocolVersion, Kind: "bootstrap", Metadata: testMetadata(), Policy: []byte(`{}`), Credentials: []Credential{{Reference: "workspace-vault://x", Value: []byte("secret")}}}
	if err := writeFrame(writer, MaxBootstrapBytes, bootstrap); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	if err := <-result; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("default child adapter unexpectedly available: %v", err)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

type fakePlatform struct {
	mu                 sync.Mutex
	spawn              SpawnRequest
	evidence           Evidence
	runtime            *fakeRuntime
	child              *fakeChild
	attack             string
	lastChallenge      string
	lastResponse       ControlResponse
	stallHandshake     bool
	childDead          bool
	environmentTouched bool
	filesTouched       bool
	logs               bytes.Buffer
}

func newFakePlatform() *fakePlatform {
	runtime := &fakeRuntime{address: "127.0.0.1:8123", control: "/tmp/blazn-test-control.sock", token: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", pid: 4321, start: "epoch:child", executable: "dev:1/inode:2", stopped: make(chan struct{}), handlerReady: make(chan struct{})}
	return &fakePlatform{runtime: runtime, child: &fakeChild{pid: 4321, done: make(chan struct{})}, evidence: Evidence{PID: 4321, OwnerUID: 1000, ProcessStartIdentity: "epoch:child", ExecutablePath: "/usr/local/bin/blazn", ExecutableIdentity: "dev:1/inode:2", BinaryDigest: testDigest}}
}

func (p *fakePlatform) Spawn(ctx context.Context, request SpawnRequest) (Child, error) {
	p.spawn = request
	if p.childDead {
		_ = request.Handshake.(io.Closer).Close()
		close(p.child.done)
		return p.child, nil
	}
	if p.stallHandshake {
		return p.child, nil
	}
	go func() {
		err := RunChild(ctx, request.Bootstrap.(io.ReadCloser), request.Handshake.(io.WriteCloser), RuntimeFactoryFunc(func(context.Context, Bootstrap) (Runtime, error) { return p.runtime, nil }))
		p.child.finish(err)
	}()
	return p.child, nil
}

func (p *fakePlatform) Evidence(context.Context, int) (Evidence, bool, error) {
	select {
	case <-p.runtime.stopped:
		return Evidence{}, false, nil
	default:
	}
	return p.evidence, true, nil
}

func (p *fakePlatform) DialControl(ctx context.Context, _ int, address string) (io.ReadWriteCloser, error) {
	if address != p.runtime.control {
		return nil, ErrUnauthorized
	}
	if p.attack == "socket" {
		client, server := net.Pipe()
		go func() { defer server.Close(); <-ctx.Done() }()
		return client, nil
	}
	select {
	case <-p.runtime.handlerReady:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		var request ControlRequest
		if readFrame(server, MaxControlBytes, &request) != nil {
			return
		}
		p.lastChallenge = request.Challenge
		response, err := p.runtime.handler(ctx, request)
		if err != nil {
			return
		}
		switch p.attack {
		case "replay":
			response.Challenge = testNonce
		case "key":
			public, _, _ := ed25519.GenerateKey(rand.Reader)
			response.PublicKey = base64.RawURLEncoding.EncodeToString(public)
		case "proof":
			response.Proof.Generation++
		}
		_ = writeFrame(server, MaxControlBytes, response)
	}()
	return client, nil
}

type fakeRuntime struct {
	address, control, token string
	pid                     int
	start, executable       string
	stopped                 chan struct{}
	handlerReady            chan struct{}
	handler                 func(context.Context, ControlRequest) (ControlResponse, error)
	once                    sync.Once
}

func (r *fakeRuntime) Address() string                 { return r.address }
func (r *fakeRuntime) ControlAddress() string          { return r.control }
func (r *fakeRuntime) ListenerToken() string           { return r.token }
func (r *fakeRuntime) Identity() (int, string, string) { return r.pid, r.start, r.executable }
func (r *fakeRuntime) ServeControl(ctx context.Context, handler func(context.Context, ControlRequest) (ControlResponse, error)) error {
	r.handler = handler
	close(r.handlerReady)
	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *fakeRuntime) Shutdown(context.Context) error {
	r.once.Do(func() { close(r.stopped) })
	return nil
}

type fakeChild struct {
	mu                 sync.Mutex
	pid                int
	done               chan struct{}
	err                error
	terminated, killed int
	once               sync.Once
}

func (c *fakeChild) PID() int         { return c.pid }
func (c *fakeChild) Terminate() error { c.mu.Lock(); c.terminated++; c.mu.Unlock(); return nil }
func (c *fakeChild) Kill() error {
	c.mu.Lock()
	c.killed++
	c.mu.Unlock()
	c.finish(errors.New("killed"))
	return nil
}
func (c *fakeChild) finish(err error) { c.once.Do(func() { c.err = err; close(c.done) }) }
func (c *fakeChild) Wait(ctx context.Context) error {
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestWireProofRoundTripCoversEveryAuthorityField(t *testing.T) {
	proof := state.LiveListenerProof{PID: 1, ProcessStartIdentity: "start", ExecutableIdentity: "dev:1/inode:2", BinaryDigest: testDigest, ListenerKeyFingerprint: testDigest, ActivationNonce: testNonce, OwnerUID: 1000, Generation: 3, Mode: "session", SessionIdentity: "session"}
	if got := proofFromWire(proofToWire(proof)); got != proof {
		t.Fatalf("proof lost authority fields: %#v", got)
	}
}
