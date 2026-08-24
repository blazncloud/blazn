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

func TestChildRejectsUnauthenticatedReplayedWrongTokenAndTamperedControl(t *testing.T) {
	platform := newFakePlatform()
	controller := &Controller{Platform: platform, HandshakeTimeout: time.Second, ControlTimeout: time.Second}
	managed, err := controller.Start(context.Background(), testStartRequest())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-platform.runtime.handlerReady:
	case <-time.After(time.Second):
		t.Fatal("control handler was not ready")
	}
	wire := proofToWire(managed.Proof())
	challenge, err := freshChallenge()
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated := ControlRequest{Version: ProtocolVersion, Kind: "control_request", Action: "stop", Challenge: challenge}
	if _, err := platform.runtime.handler(context.Background(), unauthenticated); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthenticated stop returned %v", err)
	}
	assertRuntimeLive(t, platform.runtime)

	wrongToken := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	wrongMAC, err := authenticateControlRequest(wrongToken, "stop", challenge, wire)
	if err != nil {
		t.Fatal(err)
	}
	wrong := unauthenticated
	wrong.Authenticator = wrongMAC
	if _, err := platform.runtime.handler(context.Background(), wrong); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong-token stop returned %v", err)
	}
	assertRuntimeLive(t, platform.runtime)

	inspectMAC, err := authenticateControlRequest(managed.ListenerToken(), "inspect", challenge, wire)
	if err != nil {
		t.Fatal(err)
	}
	tampered := unauthenticated
	tampered.Authenticator = inspectMAC
	if _, err := platform.runtime.handler(context.Background(), tampered); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered-action stop returned %v", err)
	}
	assertRuntimeLive(t, platform.runtime)

	replayChallenge, err := freshChallenge()
	if err != nil {
		t.Fatal(err)
	}
	replayMAC, err := authenticateControlRequest(managed.ListenerToken(), "inspect", replayChallenge, wire)
	if err != nil {
		t.Fatal(err)
	}
	replay := ControlRequest{Version: ProtocolVersion, Kind: "control_request", Action: "inspect", Challenge: replayChallenge, Authenticator: replayMAC}
	if _, err := platform.runtime.handler(context.Background(), replay); err != nil {
		t.Fatalf("first authenticated request returned %v", err)
	}
	if _, err := platform.runtime.handler(context.Background(), replay); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replayed request returned %v", err)
	}
	assertRuntimeLive(t, platform.runtime)
}

func assertRuntimeLive(t *testing.T, runtime *fakeRuntime) {
	t.Helper()
	select {
	case <-runtime.stopped:
		t.Fatal("unauthorized request stopped the runtime")
	default:
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

func TestRestartDiscoveryReauthenticatesPersistedMaterialWithoutPersistence(t *testing.T) {
	platform := newFakePlatform()
	starter := &Controller{Platform: platform, HandshakeTimeout: time.Second, ControlTimeout: time.Second}
	managed, err := starter.Start(context.Background(), testStartRequest())
	if err != nil {
		t.Fatal(err)
	}
	starter.mu.Lock()
	known := starter.known[managed.Proof().PID]
	starter.mu.Unlock()
	persisted := PersistedListener{
		Proof:          managed.Proof(),
		ControlAddress: known.controlAddress,
		ExecutablePath: known.executablePath,
		PublicKey:      known.publicKey,
		ListenerToken:  known.listenerToken,
	}
	discovery := &Controller{Platform: platform, ControlTimeout: time.Second}
	proof, live, err := discovery.Discover(context.Background(), persisted)
	if err != nil || !live || proof != managed.Proof() {
		t.Fatalf("discover failed: live=%t proof=%#v err=%v", live, proof, err)
	}
	if proof, live, err := discovery.Inspect(context.Background(), managed.Proof().PID); err != nil || !live || proof != managed.Proof() {
		t.Fatalf("discovered listener was not registered: live=%t proof=%#v err=%v", live, proof, err)
	}

	mutations := map[string]func(*PersistedListener, *fakePlatform){
		"pid reuse": func(_ *PersistedListener, value *fakePlatform) { value.evidence.ProcessStartIdentity = "reused" },
		"uid":       func(_ *PersistedListener, value *fakePlatform) { value.evidence.OwnerUID++ },
		"path":      func(_ *PersistedListener, value *fakePlatform) { value.evidence.ExecutablePath = "/tmp/substitute" },
		"inode":     func(_ *PersistedListener, value *fakePlatform) { value.evidence.ExecutableIdentity = "dev:9/inode:9" },
		"sha": func(_ *PersistedListener, value *fakePlatform) {
			value.evidence.BinaryDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
		"key": func(value *PersistedListener, _ *fakePlatform) {
			value.PublicKey = strings.Repeat("A", len(value.PublicKey))
		},
		"token": func(value *PersistedListener, _ *fakePlatform) {
			value.ListenerToken = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
		},
		"socket": func(value *PersistedListener, _ *fakePlatform) { value.ControlAddress = "/tmp/replaced.sock" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := persisted
			candidatePlatform := newFakePlatform()
			// Reuse the live authenticated runtime/public key from the starter.
			candidatePlatform.runtime = platform.runtime
			candidatePlatform.child = platform.child
			candidatePlatform.evidence = platform.evidence
			mutate(&candidate, candidatePlatform)
			controller := &Controller{Platform: candidatePlatform, ControlTimeout: 50 * time.Millisecond}
			if _, live, err := controller.Discover(context.Background(), candidate); err == nil || live {
				t.Fatalf("substituted persisted material accepted: live=%t err=%v", live, err)
			}
			controller.mu.Lock()
			_, registered := controller.known[candidate.Proof.PID]
			controller.mu.Unlock()
			if registered {
				t.Fatal("failed discovery registered listener")
			}
		})
	}
}

func TestRestartDiscoveredStopWaitsForExactExitAndRetainsAuthorityOnConflict(t *testing.T) {
	tests := map[string]struct {
		afterStop func(*Evidence, int) (bool, error)
		wantErr   error
		minPolls  int
	}{
		"asynchronous exit": {
			afterStop: func(_ *Evidence, poll int) (bool, error) { return poll < 3, nil },
			minPolls:  3,
		},
		"stalled exit": {
			afterStop: func(_ *Evidence, _ int) (bool, error) { return true, nil },
			wantErr:   context.DeadlineExceeded,
		},
		"pid reuse": {
			afterStop: func(evidence *Evidence, _ int) (bool, error) {
				evidence.ProcessStartIdentity = "epoch:reused"
				return true, nil
			},
			wantErr: ErrUnauthorized,
		},
		"executable substitution": {
			afterStop: func(evidence *Evidence, _ int) (bool, error) {
				evidence.ExecutableIdentity = "dev:9/inode:9"
				return true, nil
			},
			wantErr: ErrUnauthorized,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			base := newFakePlatform()
			platform := &restartEvidencePlatform{fakePlatform: base}
			starter := &Controller{Platform: platform, HandshakeTimeout: time.Second, ControlTimeout: time.Second}
			managed, err := starter.Start(context.Background(), testStartRequest())
			if err != nil {
				t.Fatal(err)
			}
			starter.mu.Lock()
			known := starter.known[managed.Proof().PID]
			starter.mu.Unlock()
			discovery := &Controller{Platform: platform, ControlTimeout: time.Second, StopGrace: 35 * time.Millisecond}
			persisted := PersistedListener{Proof: managed.Proof(), ControlAddress: known.controlAddress, ExecutablePath: known.executablePath, PublicKey: known.publicKey, ListenerToken: known.listenerToken}
			if _, live, err := discovery.Discover(context.Background(), persisted); err != nil || !live {
				t.Fatalf("discover failed: live=%t err=%v", live, err)
			}
			platform.afterStop = test.afterStop
			err = discovery.Stop(context.Background(), managed.Proof())
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("stop failed: %v", err)
				}
			} else if !errors.Is(err, test.wantErr) {
				t.Fatalf("stop returned %v, want %v", err, test.wantErr)
			}
			if platform.polls < test.minPolls {
				t.Fatalf("exit evidence polls=%d, want at least %d", platform.polls, test.minPolls)
			}
			discovery.mu.Lock()
			_, retained := discovery.known[managed.Proof().PID]
			discovery.mu.Unlock()
			if retained != (test.wantErr != nil) {
				t.Fatalf("authority retained=%t after err=%v", retained, err)
			}
			base.child.mu.Lock()
			terminated, killed := base.child.terminated, base.child.killed
			base.child.mu.Unlock()
			if terminated != 0 || killed != 0 {
				t.Fatalf("restart discovery signaled a process: terminate=%d kill=%d", terminated, killed)
			}
		})
	}
}

func TestControllerRejectsIncompleteProcessIdentity(t *testing.T) {
	mutations := map[string]func(*fakePlatform){
		"zero child pid":               func(p *fakePlatform) { p.child.pid = 0; p.runtime.pid = 0; p.evidence.PID = 0 },
		"empty runtime start":          func(p *fakePlatform) { p.runtime.start = ""; p.evidence.ProcessStartIdentity = "" },
		"empty evidence start":         func(p *fakePlatform) { p.evidence.ProcessStartIdentity = "" },
		"empty executable path":        func(p *fakePlatform) { p.evidence.ExecutablePath = "" },
		"relative executable path":     func(p *fakePlatform) { p.evidence.ExecutablePath = "usr/local/bin/blazn" },
		"empty runtime dev inode":      func(p *fakePlatform) { p.runtime.executable = ""; p.evidence.ExecutableIdentity = "" },
		"malformed evidence dev inode": func(p *fakePlatform) { p.evidence.ExecutableIdentity = "inode-only" },
		"empty evidence sha":           func(p *fakePlatform) { p.evidence.BinaryDigest = "" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			platform := newFakePlatform()
			mutate(platform)
			controller := &Controller{Platform: platform, HandshakeTimeout: time.Second, StopGrace: 20 * time.Millisecond}
			if _, err := controller.Start(context.Background(), testStartRequest()); err == nil {
				t.Fatal("incomplete process identity was accepted")
			}
		})
	}
}

func TestFramePayloadsAndDecodedCredentialsAreZeroed(t *testing.T) {
	bootstrap := Bootstrap{Version: ProtocolVersion, Kind: "bootstrap", Challenge: testNonce, Metadata: testMetadata(), Policy: []byte(`{}`), Credentials: []Credential{{Reference: "workspace-vault://x", Value: []byte("destination-secret")}}}

	t.Run("writer success", func(t *testing.T) {
		var observed []byte
		if err := writeFrameObserved(io.Discard, MaxBootstrapBytes, bootstrap, func(value []byte) { observed = value }); err != nil {
			t.Fatal(err)
		}
		assertZeroed(t, observed)
	})
	t.Run("writer error", func(t *testing.T) {
		var observed []byte
		err := writeFrameObserved(errorWriter{}, MaxBootstrapBytes, bootstrap, func(value []byte) { observed = value })
		if err == nil {
			t.Fatal("failing writer succeeded")
		}
		assertZeroed(t, observed)
	})
	t.Run("writer oversize", func(t *testing.T) {
		var observed []byte
		err := writeFrameObserved(io.Discard, 2, bootstrap, func(value []byte) { observed = value })
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("oversize write returned %v", err)
		}
		assertZeroed(t, observed)
	})
	t.Run("reader success", func(t *testing.T) {
		frame := encodedFrame(t, bootstrap)
		var observed []byte
		var decoded Bootstrap
		if err := readFrameObserved(bytes.NewReader(frame), MaxBootstrapBytes, &decoded, func(value []byte) { observed = value }); err != nil {
			t.Fatal(err)
		}
		assertZeroed(t, observed)
		zeroBootstrap(&decoded)
		assertZeroed(t, decoded.Credentials[0].Value)
	})
	t.Run("reader partial", func(t *testing.T) {
		frame := encodedFrame(t, bootstrap)
		var observed []byte
		var decoded Bootstrap
		err := readFrameObserved(bytes.NewReader(frame[:len(frame)-3]), MaxBootstrapBytes, &decoded, func(value []byte) { observed = value })
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("partial read returned %v", err)
		}
		assertZeroed(t, observed)
	})
	t.Run("decoded credential factory error", func(t *testing.T) {
		frame := encodedFrame(t, bootstrap)
		var retained []byte
		err := RunChild(context.Background(), io.NopCloser(bytes.NewReader(frame)), nopWriteCloser{io.Discard}, RuntimeFactoryFunc(func(_ context.Context, value Bootstrap) (Runtime, error) {
			retained = value.Credentials[0].Value
			return nil, errors.New("fixture factory failure")
		}))
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("factory error returned %v", err)
		}
		assertZeroed(t, retained)
	})
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("fixture write failure") }

func encodedFrame(t *testing.T, value any) []byte {
	t.Helper()
	var frame bytes.Buffer
	if err := writeFrame(&frame, MaxBootstrapBytes, value); err != nil {
		t.Fatal(err)
	}
	return frame.Bytes()
}

func assertZeroed(t *testing.T, value []byte) {
	t.Helper()
	if len(value) == 0 {
		t.Fatal("zeroing observer captured no buffer")
	}
	for _, item := range value {
		if item != 0 {
			t.Fatalf("buffer retained non-zero byte %x", item)
		}
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

func TestStartAndControlWritesRemainBounded(t *testing.T) {
	t.Run("child handshakes before consuming bootstrap", func(t *testing.T) {
		platform := newFakePlatform()
		platform.earlyHandshake = true
		controller := &Controller{Platform: platform, HandshakeTimeout: 20 * time.Millisecond, StopGrace: 20 * time.Millisecond}
		started := time.Now()
		if _, err := controller.Start(context.Background(), testStartRequest()); err == nil {
			t.Fatal("incomplete bootstrap write succeeded")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("bootstrap write exceeded bound: %s", elapsed)
		}
		_ = platform.spawn.Bootstrap.(io.Closer).Close()
		_ = platform.spawn.Handshake.(io.Closer).Close()
	})

	t.Run("control peer never reads", func(t *testing.T) {
		platform := newFakePlatform()
		controller := &Controller{Platform: platform, HandshakeTimeout: time.Second, ControlTimeout: 20 * time.Millisecond}
		managed, err := controller.Start(context.Background(), testStartRequest())
		if err != nil {
			t.Fatal(err)
		}
		platform.stallControlWrite = true
		started := time.Now()
		if _, live, err := controller.Inspect(context.Background(), managed.Identity().PID); err == nil || live {
			t.Fatalf("stalled control write returned live=%t err=%v", live, err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("control write exceeded bound: %s", elapsed)
		}
	})
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
	bootstrap := Bootstrap{Version: ProtocolVersion, Kind: "bootstrap", Challenge: testNonce, Metadata: testMetadata(), Policy: []byte(`{}`), Credentials: []Credential{{Reference: "workspace-vault://x", Value: []byte("secret")}}}
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
	earlyHandshake     bool
	stallControlWrite  bool
	childDead          bool
	environmentTouched bool
	filesTouched       bool
	logs               bytes.Buffer
}

type restartEvidencePlatform struct {
	*fakePlatform
	afterStop func(*Evidence, int) (bool, error)
	polls     int
}

func (p *restartEvidencePlatform) Evidence(ctx context.Context, pid int) (Evidence, bool, error) {
	select {
	case <-p.runtime.stopped:
		p.polls++
		evidence := p.evidence
		if p.afterStop == nil {
			return Evidence{}, false, nil
		}
		live, err := p.afterStop(&evidence, p.polls)
		if !live || err != nil {
			return Evidence{}, live, err
		}
		return evidence, true, nil
	default:
		return p.fakePlatform.Evidence(ctx, pid)
	}
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
	if p.earlyHandshake {
		go p.writeEarlyHandshake(request)
		return p.child, nil
	}
	go func() {
		err := RunChild(ctx, request.Bootstrap.(io.ReadCloser), request.Handshake.(io.WriteCloser), RuntimeFactoryFunc(func(context.Context, Bootstrap) (Runtime, error) { return p.runtime, nil }))
		p.child.finish(err)
	}()
	return p.child, nil
}

func (p *fakePlatform) writeEarlyHandshake(request SpawnRequest) {
	var size [4]byte
	if _, err := io.ReadFull(request.Bootstrap, size[:]); err != nil {
		return
	}
	prefix := make([]byte, 160)
	count, err := io.ReadFull(request.Bootstrap, prefix)
	if err != nil {
		return
	}
	prefix = prefix[:count]
	marker := []byte(`"challenge":"`)
	start := bytes.Index(prefix, marker)
	if start < 0 || start+len(marker)+len(testNonce) > len(prefix) {
		return
	}
	challenge := string(prefix[start+len(marker) : start+len(marker)+len(testNonce)])
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return
	}
	proof := state.LiveListenerProof{PID: p.runtime.pid, ProcessStartIdentity: p.runtime.start, ExecutableIdentity: p.runtime.executable, BinaryDigest: testDigest, ListenerKeyFingerprint: fingerprint(public), ActivationNonce: testNonce, OwnerUID: 1000, Generation: 2, Mode: "session", SessionIdentity: "uid:1000/session:test"}
	wire := proofToWire(proof)
	signature, err := signResponse(private, "handshake", "start", challenge, wire, true)
	zeroBytes(private)
	if err != nil {
		return
	}
	_ = writeFrame(request.Handshake, MaxHandshakeBytes, Handshake{Version: ProtocolVersion, Kind: "handshake", Address: p.runtime.address, ControlAddress: p.runtime.control, ListenerToken: p.runtime.token, PublicKey: base64.RawURLEncoding.EncodeToString(public), Proof: wire, Challenge: challenge, Signature: signature})
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
	if p.stallControlWrite {
		return newBlockingConnection(), nil
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

type blockingConnection struct {
	once   sync.Once
	closed chan struct{}
}

func newBlockingConnection() *blockingConnection {
	return &blockingConnection{closed: make(chan struct{})}
}

func (c *blockingConnection) Read([]byte) (int, error) {
	<-c.closed
	return 0, io.ErrClosedPipe
}

func (c *blockingConnection) Write([]byte) (int, error) {
	<-c.closed
	return 0, io.ErrClosedPipe
}

func (c *blockingConnection) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
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
