package node

import (
	"errors"
	"os"
	"runtime"
	"strings"
)

// HostMachineFingerprint returns a stable, non-secret binding for a supported
// Linux host. The raw machine ID never leaves this process.
func HostMachineFingerprint() (string, error) {
	return hostMachineFingerprint(runtime.GOOS, runtime.GOARCH, os.ReadFile)
}

func hostMachineFingerprint(goos, goarch string, readFile func(string) ([]byte, error)) (string, error) {
	if goos != "linux" {
		return "", errors.New("automatic machine fingerprinting is currently supported only on Linux")
	}
	machineID, err := readFile("/etc/machine-id")
	if err != nil {
		return "", errors.New("read stable Linux machine identity")
	}
	value := strings.ToLower(strings.TrimSpace(string(machineID)))
	if len(value) != 32 {
		return "", errors.New("Linux machine identity is invalid")
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return "", errors.New("Linux machine identity is invalid")
		}
	}
	return MachineFingerprint("linux", goarch, value), nil
}
