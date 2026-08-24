//go:build darwin || linux

package process

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type ProcessRecord struct {
	PID           int
	UID           int
	StartIdentity string
	Executable    string
}

type ProcessView interface {
	Lookup(context.Context, int) (ProcessRecord, bool, error)
}

type UnixPlatform struct {
	View        ProcessView
	UID         int
	DialTimeout time.Duration
	Dial        func(context.Context, string, string) (net.Conn, error)
	Command     func(context.Context, string, ...string) *exec.Cmd
}

func NewUnixPlatform() *UnixPlatform {
	return &UnixPlatform{View: nativeProcessView{}, UID: os.Getuid(), DialTimeout: DefaultControlTTL, Dial: (&net.Dialer{}).DialContext}
}

func (p *UnixPlatform) Spawn(ctx context.Context, request SpawnRequest) (Child, error) {
	if p == nil || request.Bootstrap == nil || request.Handshake == nil || len(request.Argv) != 3 || request.Argv[0] != request.Executable || request.Argv[1] != ChildCommand || request.Argv[2] != ProtocolVersion {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, safeError(err)
	}
	executable, executableFile, err := openVerifiedCurrentExecutable(request.Executable)
	if err != nil {
		return nil, ErrUnauthorized
	}
	defer executableFile.Close()
	bootstrapRead, bootstrapWrite, err := os.Pipe()
	if err != nil {
		return nil, safeError(err)
	}
	handshakeRead, handshakeWrite, err := os.Pipe()
	if err != nil {
		_ = bootstrapRead.Close()
		_ = bootstrapWrite.Close()
		return nil, safeError(err)
	}
	closeAll := func() {
		_ = bootstrapRead.Close()
		_ = bootstrapWrite.Close()
		_ = handshakeRead.Close()
		_ = handshakeWrite.Close()
	}
	commandFactory := p.Command
	var command *exec.Cmd
	if commandFactory == nil {
		// Start cancellation is bounded by the controller handshake. Once exec
		// succeeds the detached listener must not inherit the caller's context.
		command = exec.Command(executableFDPath(), ChildCommand, ProtocolVersion)
	} else {
		command = commandFactory(ctx, executable, ChildCommand, ProtocolVersion)
	}
	if command == nil {
		closeAll()
		return nil, ErrUnavailable
	}
	command.Args[0] = executable
	command.Env = []string{}
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	command.ExtraFiles = []*os.File{bootstrapRead, handshakeWrite, executableFile}
	command.SysProcAttr = detachedProcessAttributes()
	if err := command.Start(); err != nil {
		closeAll()
		return nil, safeError(err)
	}
	_ = bootstrapRead.Close()
	_ = handshakeWrite.Close()
	go func() {
		_, _ = io.Copy(bootstrapWrite, request.Bootstrap)
		_ = bootstrapWrite.Close()
	}()
	go func() {
		_, _ = io.Copy(request.Handshake, handshakeRead)
		_ = handshakeRead.Close()
		if closer, ok := request.Handshake.(io.Closer); ok {
			_ = closer.Close()
		}
	}()
	return &execChild{command: command}, nil
}

func (p *UnixPlatform) Evidence(ctx context.Context, pid int) (Evidence, bool, error) {
	if p == nil || p.View == nil || pid < 1 {
		return Evidence{}, false, ErrUnavailable
	}
	record, live, err := p.View.Lookup(ctx, pid)
	if err != nil || !live {
		return Evidence{}, live, safeError(err)
	}
	uid := p.UID
	if uid < 0 {
		uid = os.Getuid()
	}
	if record.PID != pid || record.UID != uid || !validOpaqueIdentity(record.StartIdentity) {
		return Evidence{}, false, ErrUnauthorized
	}
	canonical, identity, digest, err := inspectExecutable(record.Executable)
	if err != nil {
		return Evidence{}, false, ErrUnauthorized
	}
	return Evidence{PID: pid, OwnerUID: record.UID, ProcessStartIdentity: record.StartIdentity, ExecutablePath: canonical, ExecutableIdentity: identity, BinaryDigest: digest}, true, nil
}

func (p *UnixPlatform) DialControl(ctx context.Context, _ int, address string) (io.ReadWriteCloser, error) {
	if p == nil || !filepath.IsAbs(address) || filepath.Clean(address) != address {
		return nil, ErrUnauthorized
	}
	info, err := os.Lstat(address)
	uid := p.UID
	if uid < 0 {
		uid = os.Getuid()
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || statUID(info) != uid {
		return nil, ErrUnauthorized
	}
	timeout := p.DialTimeout
	if timeout <= 0 {
		timeout = DefaultControlTTL
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	connection, err := dial(dialCtx, "unix", address)
	if err != nil {
		return nil, safeError(err)
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		_ = connection.Close()
		return nil, ErrUnauthorized
	}
	peer, err := unixPeerUID(unixConnection)
	after, afterErr := os.Lstat(address)
	if err != nil || peer != uid || afterErr != nil || !os.SameFile(info, after) || after.Mode()&os.ModeSocket == 0 || after.Mode().Perm() != 0600 || statUID(after) != uid {
		_ = connection.Close()
		return nil, ErrUnauthorized
	}
	_ = connection.SetDeadline(time.Now().Add(timeout))
	return connection, nil
}

type execChild struct {
	command *exec.Cmd
	wait    sync.Once
	done    chan struct{}
	err     error
}

func (c *execChild) PID() int {
	if c == nil || c.command == nil || c.command.Process == nil {
		return 0
	}
	return c.command.Process.Pid
}

func (c *execChild) Terminate() error {
	if c.PID() == 0 {
		return nil
	}
	return c.command.Process.Signal(syscall.SIGTERM)
}

func (c *execChild) Kill() error {
	if c.PID() == 0 {
		return nil
	}
	return c.command.Process.Kill()
}

func (c *execChild) Wait(ctx context.Context) error {
	if c == nil || c.command == nil {
		return nil
	}
	c.wait.Do(func() {
		c.done = make(chan struct{})
		go func() {
			c.err = c.command.Wait()
			close(c.done)
		}()
	})
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func openVerifiedCurrentExecutable(requested string) (string, *os.File, error) {
	current, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return "", nil, err
	}
	requested, err = filepath.EvalSymlinks(requested)
	if err != nil || requested != current || !filepath.IsAbs(requested) {
		return "", nil, ErrUnauthorized
	}
	executable, err := os.Open(current)
	if err != nil {
		return "", nil, err
	}
	currentInfo, err := executable.Stat()
	if err != nil || !currentInfo.Mode().IsRegular() {
		_ = executable.Close()
		return "", nil, ErrUnauthorized
	}
	requestedInfo, err := os.Stat(requested)
	if err != nil || !os.SameFile(currentInfo, requestedInfo) {
		_ = executable.Close()
		return "", nil, ErrUnauthorized
	}
	return requested, executable, nil
}

func inspectExecutable(path string) (string, string, string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(canonical) {
		return "", "", "", ErrUnauthorized
	}
	file, err := os.Open(canonical)
	if err != nil {
		return "", "", "", err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		_ = file.Close()
		return "", "", "", ErrUnauthorized
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	pathInfo, statErr := os.Stat(canonical)
	if err := errors.Join(copyErr, closeErr, statErr); err != nil || !os.SameFile(opened, pathInfo) {
		return "", "", "", ErrUnauthorized
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok {
		return "", "", "", ErrUnavailable
	}
	return canonical, fmt.Sprintf("dev:%d/inode:%d", stat.Dev, stat.Ino), "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func statUID(info os.FileInfo) int {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}

func processStartIdentity(sec, subsec uint64) string {
	return "start:" + strconv.FormatUint(sec, 10) + ":" + strconv.FormatUint(subsec, 10)
}
