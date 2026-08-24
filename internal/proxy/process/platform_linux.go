//go:build linux

package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type nativeProcessView struct{}

func (nativeProcessView) Lookup(ctx context.Context, pid int) (ProcessRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return ProcessRecord{}, false, err
	}
	root := filepath.Join("/proc", strconv.Itoa(pid))
	statRaw, err := os.ReadFile(filepath.Join(root, "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return ProcessRecord{}, false, nil
	}
	if err != nil {
		return ProcessRecord{}, false, err
	}
	end := strings.LastIndexByte(string(statRaw), ')')
	if end < 0 {
		return ProcessRecord{}, false, ErrUnauthorized
	}
	fields := strings.Fields(string(statRaw[end+1:]))
	// After comm, field zero is state; process start time is original field 22.
	if len(fields) < 20 {
		return ProcessRecord{}, false, ErrUnauthorized
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || startTicks == 0 {
		return ProcessRecord{}, false, ErrUnauthorized
	}
	status, err := os.Open(filepath.Join(root, "status"))
	if err != nil {
		return ProcessRecord{}, false, err
	}
	defer status.Close()
	uid := -1
	scanner := bufio.NewScanner(status)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Uid:") {
			parts := strings.Fields(strings.TrimPrefix(line, "Uid:"))
			if len(parts) != 4 {
				return ProcessRecord{}, false, ErrUnauthorized
			}
			uid, err = strconv.Atoi(parts[0])
			if err != nil {
				return ProcessRecord{}, false, ErrUnauthorized
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return ProcessRecord{}, false, err
	}
	if uid < 0 {
		return ProcessRecord{}, false, ErrUnauthorized
	}
	executable, err := os.Readlink(filepath.Join(root, "exe"))
	if err != nil {
		return ProcessRecord{}, false, err
	}
	if strings.HasSuffix(executable, " (deleted)") {
		return ProcessRecord{}, false, ErrUnauthorized
	}
	return ProcessRecord{PID: pid, UID: uid, StartIdentity: fmt.Sprintf("linux-start-ticks:%d", startTicks), Executable: executable}, true, nil
}

func detachedProcessAttributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

func executableFDPath() string { return "/proc/self/fd/5" }

func unixPeerUID(connection *net.UnixConn) (int, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return -1, err
	}
	uid := -1
	var socketErr error
	err = raw.Control(func(fd uintptr) {
		credential, currentErr := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if currentErr != nil {
			socketErr = currentErr
			return
		}
		uid = int(credential.Uid)
	})
	return uid, errors.Join(err, socketErr)
}
