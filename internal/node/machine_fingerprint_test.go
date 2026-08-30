package node

import (
	"errors"
	"strings"
	"testing"
)

func TestHostMachineFingerprintIsStableAndDoesNotExposeMachineID(t *testing.T) {
	machineID := "0123456789abcdef0123456789abcdef"
	read := func(path string) ([]byte, error) {
		if path != "/etc/machine-id" {
			t.Fatalf("path=%q", path)
		}
		return []byte(machineID + "\n"), nil
	}
	got, err := hostMachineFingerprint("linux", "amd64", read)
	want := MachineFingerprint("linux", "amd64", machineID)
	if err != nil || got != want || strings.Contains(got, machineID) || !validMachineFingerprint(got) {
		t.Fatalf("got=%q want=%q err=%v", got, want, err)
	}
}

func TestHostMachineFingerprintFailsClosed(t *testing.T) {
	if _, err := hostMachineFingerprint("darwin", "arm64", func(string) ([]byte, error) { return nil, nil }); err == nil {
		t.Fatal("unsupported platform accepted")
	}
	if _, err := hostMachineFingerprint("linux", "amd64", func(string) ([]byte, error) { return nil, errors.New("missing") }); err == nil {
		t.Fatal("missing machine ID accepted")
	}
	if _, err := hostMachineFingerprint("linux", "amd64", func(string) ([]byte, error) { return []byte("not-an-id"), nil }); err == nil {
		t.Fatal("invalid machine ID accepted")
	}
}
