//go:build darwin

package process

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinNativeSpawnFailsClosedBeforeProcessCreation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	child, err := NewUnixPlatform().Spawn(context.Background(), SpawnRequest{
		Executable: executable,
		Argv:       []string{executable, ChildCommand, ProtocolVersion},
		Bootstrap:  strings.NewReader("unused"),
		Handshake:  io.Discard,
	})
	if child != nil || !errors.Is(err, ErrSpawnUnsupported) {
		t.Fatalf("Darwin native spawn did not fail closed: child=%v err=%v", child, err)
	}
}
