//go:build linux

package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if IsChildInvocation(os.Args[1:]) {
		runNativeFDHelper()
		return
	}
	os.Exit(m.Run())
}

func runNativeFDHelper() {
	bootstrap := os.NewFile(3, "bootstrap")
	handshake := os.NewFile(4, "handshake")
	executable := os.NewFile(5, "executable")
	if bootstrap == nil || handshake == nil || executable == nil {
		os.Exit(91)
	}
	info, err := executable.Stat()
	if err != nil || !info.Mode().IsRegular() {
		os.Exit(92)
	}
	value, err := io.ReadAll(bootstrap)
	if err != nil {
		os.Exit(93)
	}
	if _, err := fmt.Fprintf(handshake, "fd3=%s;fd5=regular;env=%d", value, len(os.Environ())); err != nil {
		os.Exit(94)
	}
	_ = handshake.Close()
	os.Exit(0)
}

func TestLinuxNativeSpawnExecutesVerifiedFD5(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	secret := "native-fd-bootstrap-secret"
	handshakeRead, handshakeWrite := io.Pipe()
	child, err := NewUnixPlatform().Spawn(context.Background(), SpawnRequest{
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
		t.Fatalf("native fd-backed child received unexpected authority: %q", result)
	}
}
