//go:build darwin || linux

package plugin

import (
	"os"
	"syscall"
)

func newBrokerSocketPair() (*os.File, *os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	return os.NewFile(uintptr(fds[0]), "blazn-plugin-broker-root"), os.NewFile(uintptr(fds[1]), "blazn-plugin-broker-child"), nil
}
