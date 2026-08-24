//go:build darwin

package process

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

const procPIDTBSDInfo = 3

type darwinBSDInfo struct {
	Flags, Status, XStatus, PID, PPID, UID, GID, RUID, RGID, SVUID, SVGID, RFU uint32
	Comm                                                                       [16]byte
	Name                                                                       [32]byte
	NFiles, PGID, PJobC, ETDev, ETPGID                                         uint32
	Nice                                                                       int32
	StartSec, StartUSec                                                        uint64
}

type nativeProcessView struct{}

func (nativeProcessView) Lookup(ctx context.Context, pid int) (ProcessRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return ProcessRecord{}, false, err
	}
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return ProcessRecord{}, false, ErrUnavailable
	}
	defer purego.Dlclose(lib)
	var pidInfo func(int32, int32, uint64, unsafe.Pointer, int32) int32
	var pidPath func(int32, unsafe.Pointer, uint32) int32
	purego.RegisterLibFunc(&pidInfo, lib, "proc_pidinfo")
	purego.RegisterLibFunc(&pidPath, lib, "proc_pidpath")
	var info darwinBSDInfo
	if got := pidInfo(int32(pid), procPIDTBSDInfo, 0, unsafe.Pointer(&info), int32(unsafe.Sizeof(info))); got != int32(unsafe.Sizeof(info)) {
		if got == 0 {
			return ProcessRecord{}, false, nil
		}
		return ProcessRecord{}, false, ErrUnauthorized
	}
	path := make([]byte, 4096)
	count := pidPath(int32(pid), unsafe.Pointer(&path[0]), uint32(len(path)))
	if count <= 0 {
		return ProcessRecord{}, false, ErrUnauthorized
	}
	executable := strings.TrimRight(string(path[:count]), "\x00")
	if executable == "" {
		return ProcessRecord{}, false, ErrUnauthorized
	}
	return ProcessRecord{PID: pid, UID: int(info.UID), StartIdentity: processStartIdentity(info.StartSec, info.StartUSec), Executable: executable}, true, nil
}

func detachedProcessAttributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// Darwin's fdesc filesystem removes execute permission from /dev/fd entries,
// so execve("/dev/fd/5") fails even when fd 5 references an executable file.
// Fail closed until a separately reviewed descriptor-backed execution seam is
// available; falling back to the pathname would reintroduce a substitution
// race after verification.
func executableFDPath() (string, error) { return "", ErrSpawnUnsupported }

func unixPeerUID(connection *net.UnixConn) (int, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return -1, err
	}
	uid := uint32(0)
	gid := uint32(0)
	var socketErr error
	err = raw.Control(func(fd uintptr) {
		lib, openErr := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if openErr != nil {
			socketErr = openErr
			return
		}
		defer purego.Dlclose(lib)
		var getPeerEID func(int32, *uint32, *uint32) int32
		purego.RegisterLibFunc(&getPeerEID, lib, "getpeereid")
		if getPeerEID(int32(fd), &uid, &gid) != 0 {
			socketErr = syscall.EPERM
		}
	})
	return int(uid), errors.Join(err, socketErr)
}
