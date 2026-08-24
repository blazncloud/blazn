//go:build darwin || linux

package process

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixedProcessView struct {
	record ProcessRecord
	live   bool
	err    error
}

func (v fixedProcessView) Lookup(context.Context, int) (ProcessRecord, bool, error) {
	return v.record, v.live, v.err
}

func TestUnixEvidenceAuthenticatesUIDStartPathInodeDigestAndPIDReuse(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "listener")
	if err := os.WriteFile(executable, []byte("verified executable"), 0700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("verified executable"))
	canonicalExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	record := ProcessRecord{PID: 41, UID: os.Getuid(), StartIdentity: "test-start:41", Executable: executable}
	platform := &UnixPlatform{View: fixedProcessView{record: record, live: true}, UID: os.Getuid()}
	evidence, live, err := platform.Evidence(context.Background(), 41)
	if err != nil || !live {
		t.Fatalf("evidence failed: live=%t err=%v", live, err)
	}
	if evidence.PID != 41 || evidence.OwnerUID != os.Getuid() || evidence.ProcessStartIdentity != record.StartIdentity || evidence.ExecutablePath != canonicalExecutable || !strings.HasPrefix(evidence.ExecutableIdentity, "dev:") || evidence.BinaryDigest != "sha256:"+hex.EncodeToString(digest[:]) {
		t.Fatalf("unexpected evidence: %#v", evidence)
	}

	mutations := map[string]func(*ProcessRecord){
		"pid reuse": func(value *ProcessRecord) { value.PID++ },
		"uid":       func(value *ProcessRecord) { value.UID++ },
		"start":     func(value *ProcessRecord) { value.StartIdentity = "" },
		"path":      func(value *ProcessRecord) { value.Executable = "relative" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := record
			mutate(&changed)
			candidate := &UnixPlatform{View: fixedProcessView{record: changed, live: true}, UID: os.Getuid()}
			if _, live, err := candidate.Evidence(context.Background(), 41); err == nil || live {
				t.Fatalf("substituted record accepted: live=%t err=%v", live, err)
			}
		})
	}

	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(replacement, []byte("replaced executable"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, executable); err != nil {
		t.Fatal(err)
	}
	replaced, _, err := platform.Evidence(context.Background(), 41)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.ExecutableIdentity == evidence.ExecutableIdentity || replaced.BinaryDigest == evidence.BinaryDigest {
		t.Fatal("inode/digest replacement was not observed")
	}
}

func TestInspectExecutableRejectsSymlinkReplacement(t *testing.T) {
	directory := t.TempDir()
	one := filepath.Join(directory, "one")
	two := filepath.Join(directory, "two")
	link := filepath.Join(directory, "current")
	if err := os.WriteFile(one, []byte("one"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(two, []byte("two"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(one, link); err != nil {
		t.Fatal(err)
	}
	pathOne, identityOne, digestOne, err := inspectExecutable(link)
	canonicalOne, canonicalErr := filepath.EvalSymlinks(one)
	if err != nil || canonicalErr != nil || pathOne != canonicalOne {
		t.Fatalf("inspect first target: %q %v", pathOne, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(two, link); err != nil {
		t.Fatal(err)
	}
	pathTwo, identityTwo, digestTwo, err := inspectExecutable(link)
	canonicalTwo, canonicalErr := filepath.EvalSymlinks(two)
	if err != nil || canonicalErr != nil || pathTwo != canonicalTwo {
		t.Fatalf("inspect replacement: %q %v", pathTwo, err)
	}
	if identityOne == identityTwo || digestOne == digestTwo {
		t.Fatal("symlink target replacement preserved executable authority")
	}
}

func TestDialControlRequiresOwnerOnlyStableUnixSocketAndHonorsCancellation(t *testing.T) {
	directory := socketTempDir(t)
	address := filepath.Join(directory, "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(address, 0600); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *net.UnixConn, 1)
	go func() {
		connection, _ := listener.AcceptUnix()
		accepted <- connection
	}()
	platform := &UnixPlatform{UID: os.Getuid(), DialTimeout: time.Second}
	connection, err := platform.DialControl(context.Background(), os.Getpid(), address)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if peer := <-accepted; peer != nil {
		_ = peer.Close()
	}

	platform.peerUID = func(*net.UnixConn) (int, error) { return os.Getuid() + 1, nil }
	go func() {
		connection, _ := listener.AcceptUnix()
		accepted <- connection
	}()
	if _, err := platform.DialControl(context.Background(), os.Getpid(), address); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong-uid control peer returned %v", err)
	}
	if peer := <-accepted; peer != nil {
		_ = peer.Close()
	}
	platform.peerUID = unixPeerUID

	if err := os.Chmod(address, 0660); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.DialControl(context.Background(), os.Getpid(), address); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("group-writable socket returned %v", err)
	}
	if err := os.Chmod(address, 0600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "replacement.sock")
	if err := os.Symlink(address, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.DialControl(context.Background(), os.Getpid(), symlink); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("symlink socket returned %v", err)
	}

	platform.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := platform.DialControl(ctx, os.Getpid(), address); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled dial returned %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled dial exceeded its bound")
	}
}

func TestUnixSpawnUsesAnonymousFD3AndFD4WithoutSecretMetadata(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	secret := "bootstrap-secret-never-in-process-metadata"
	var capturedName string
	var capturedArgs []string
	platform := NewUnixPlatform()
	platform.Command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedName = name
		capturedArgs = append([]string(nil), args...)
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnixPlatformFDHelper$", "--", ChildCommand, ProtocolVersion)
	}
	handshakeRead, handshakeWrite := io.Pipe()
	child, err := platform.Spawn(context.Background(), SpawnRequest{
		Executable: executable,
		Argv:       []string{executable, ChildCommand, ProtocolVersion},
		Bootstrap:  strings.NewReader(secret),
		Handshake:  handshakeWrite,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := io.ReadAll(handshakeRead)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := child.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	if string(result) != "fd3="+secret+";fd5=regular;env=0" {
		t.Fatalf("helper did not receive exact anonymous-pipe bootstrap: %q", result)
	}
	if capturedName != executable || len(capturedArgs) != 2 || capturedArgs[0] != ChildCommand || capturedArgs[1] != ProtocolVersion || strings.Contains(capturedName+strings.Join(capturedArgs, " "), secret) {
		t.Fatalf("unsafe hidden invocation: name=%q args=%q", capturedName, capturedArgs)
	}
}

func TestUnixPlatformFDHelper(t *testing.T) {
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		return
	}
	bootstrap := os.NewFile(3, "bootstrap")
	handshake := os.NewFile(4, "handshake")
	executable := os.NewFile(5, "executable")
	if bootstrap == nil || handshake == nil || executable == nil {
		os.Exit(91)
	}
	info, err := executable.Stat()
	if err != nil || !info.Mode().IsRegular() {
		os.Exit(94)
	}
	value, err := io.ReadAll(bootstrap)
	if err != nil {
		os.Exit(92)
	}
	_, err = handshake.Write([]byte("fd3=" + string(value) + ";fd5=regular;env=" + string(rune('0'+len(os.Environ())))))
	if err != nil {
		os.Exit(93)
	}
	_ = handshake.Close()
	os.Exit(0)
}

func TestExecChildWaitIsReusableAfterTimeout(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestUnixWaitHelper$", "--")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	child := &execChild{command: command}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := child.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first wait returned %v", err)
	}
	if err := child.Kill(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := child.Wait(ctx); err == nil {
		t.Fatal("killed helper unexpectedly exited successfully")
	}
}

func TestUnixWaitHelper(t *testing.T) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "--" {
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
}

func socketTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "blazn-process-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}
