package microk8sissuer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const maxCommandOutput = 16 << 10
const maxTokenFileBytes = 1 << 20

var tokenLinePattern = regexp.MustCompile(`^([0-9a-f]{32})(?:\|([0-9]{10}))?$`)

type CommandRunner interface {
	Run(context.Context, string, []string) ([]byte, error)
}
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, path string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	var out bytes.Buffer
	out.Grow(maxCommandOutput)
	cmd.Stdout = &limitedWriter{w: &out, n: maxCommandOutput}
	cmd.Stderr = &limitedWriter{w: &bytes.Buffer{}, n: 1024}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("fixed MicroK8s command failed")
	}
	return out.Bytes(), nil
}

type limitedWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, fmt.Errorf("command output exceeded limit")
	}
	l.n -= len(p)
	return l.w.Write(p)
}

type MicroK8sBackend struct {
	AddNodePath, StatusPath, TokenFile string
	ExpectedUID                        uint32
	ExpectedGID                        uint32
	ExpectedMode                       os.FileMode
	Runner                             CommandRunner
	Now                                func() time.Time
	allowTestPaths                     bool
	beforeTokenExpiryWrite             func()
	syncTokenFile                      func(*os.File) error
	writeTokenExpiry                   func(*os.File, []byte, int64) (int, error)
}
type upstreamResponse struct {
	Token string   `json:"token"`
	URLs  []string `json:"urls"`
}

func (b *MicroK8sBackend) Healthy(ctx context.Context) error {
	if err := b.validateConfiguration(); err != nil {
		return err
	}
	if _, err := b.Runner.Run(ctx, b.StatusPath, []string{"--wait-ready", "--timeout", "5"}); err != nil {
		return fmt.Errorf("MicroK8s is not ready")
	}
	if err := b.validateTokenFile(false); err != nil {
		return err
	}
	if _, err := os.Lstat(b.TokenFile); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	file, unlock, err := openLockedTokenFile(ctx, b.TokenFile, b.ExpectedUID, b.ExpectedGID, b.ExpectedMode)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := readLockedTokenFile(file)
	if err != nil {
		return err
	}
	_, _, _, _, err = parseTokenFile(data, "")
	return err
}
func (b *MicroK8sBackend) validateConfiguration() error {
	if !b.allowTestPaths && (b.AddNodePath != "/snap/bin/microk8s.add-node" || b.StatusPath != "/snap/bin/microk8s.status" || b.TokenFile != "/var/snap/microk8s/current/credentials/cluster-tokens.txt") {
		return fmt.Errorf("MicroK8s paths are not allowlisted")
	}
	if !b.allowTestPaths {
		if err := validatePinnedMicroK8s(); err != nil {
			return err
		}
	}
	if b.Runner == nil {
		return fmt.Errorf("MicroK8s runner is missing")
	}
	return nil
}
func (b *MicroK8sBackend) Issue(ctx context.Context, token string, ttl int) (BackendIssue, error) {
	if err := b.Healthy(ctx); err != nil {
		return BackendIssue{}, err
	}
	if err := b.ensureTokenFile(); err != nil {
		return BackendIssue{}, err
	}
	file, unlock, err := openLockedTokenFile(ctx, b.TokenFile, b.ExpectedUID, b.ExpectedGID, b.ExpectedMode)
	if err != nil {
		return BackendIssue{}, err
	}
	defer unlock()
	data, err := readLockedTokenFile(file)
	if err != nil {
		return BackendIssue{}, err
	}
	_, count, _, _, err := parseTokenFile(data, token)
	if err != nil || count != 0 {
		return BackendIssue{}, &ProtocolError{Code: "token_collision", Message: "token file contains an ambiguous token binding"}
	}
	out, err := b.Runner.Run(ctx, b.AddNodePath, []string{"--token", token, "--token-ttl", strconv.Itoa(ttl), "--format", "json"})
	if err != nil {
		return BackendIssue{}, err
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(out, &raw) != nil || len(raw) != 2 || raw["token"] == nil || raw["urls"] == nil {
		return BackendIssue{}, fmt.Errorf("unexpected MicroK8s response")
	}
	var parsed upstreamResponse
	if json.Unmarshal(out, &parsed) != nil || !subtleTokenCheck(parsed.Token, token) {
		return BackendIssue{}, fmt.Errorf("unexpected MicroK8s credential")
	}
	for _, candidate := range parsed.URLs {
		if !validJoinURL(candidate, parsed.Token) {
			return BackendIssue{}, fmt.Errorf("unexpected MicroK8s URL")
		}
	}
	seen := make(map[string]bool, len(parsed.URLs))
	for _, candidate := range parsed.URLs {
		if seen[candidate] {
			return BackendIssue{}, fmt.Errorf("duplicate MicroK8s URL")
		}
		seen[candidate] = true
	}
	syncFile := b.syncTokenFile
	if syncFile == nil {
		syncFile = func(file *os.File) error { return file.Sync() }
	}
	if err := syncFile(file); err != nil {
		return BackendIssue{}, fmt.Errorf("persist MicroK8s token: %w", err)
	}
	data, err = readLockedTokenFile(file)
	if err != nil {
		return BackendIssue{}, err
	}
	_, count, epoch, offset, parseErr := parseTokenFile(data, token)
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	current := now().UTC()
	if parseErr != nil || count != 1 || offset < 0 || epoch <= current.Unix() || epoch > current.Add(time.Duration(ttl+5)*time.Second).Unix() || !sameTokenInode(b.TokenFile, file) {
		return BackendIssue{}, fmt.Errorf("MicroK8s token persistence is ambiguous")
	}
	return BackendIssue{TokenCheck: parsed.Token, URLs: parsed.URLs, ExpiresAt: current.Add(time.Duration(ttl) * time.Second)}, nil
}
func (b *MicroK8sBackend) Revoke(ctx context.Context, token string) error {
	if err := b.validateConfiguration(); err != nil {
		return err
	}
	if _, err := os.Lstat(b.TokenFile); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := b.validateTokenFile(true); err != nil {
		return err
	}
	return expireTokenInPlace(ctx, b.TokenFile, b.ExpectedUID, b.ExpectedGID, b.ExpectedMode, token, b.Now, b.beforeTokenExpiryWrite, b.writeTokenExpiry, b.syncTokenFile)
}
func (b *MicroK8sBackend) ensureTokenFile() error {
	if !b.allowTestPaths {
		parent, err := os.Lstat(filepath.Dir(b.TokenFile))
		if err != nil {
			return err
		}
		stat := parent.Sys().(*syscall.Stat_t)
		if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || stat.Uid != b.ExpectedUID || stat.Gid != b.ExpectedGID || parent.Mode().Perm() != 0770 {
			return fmt.Errorf("MicroK8s credential directory is unsafe")
		}
	}
	file, err := os.OpenFile(b.TokenFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, b.ExpectedMode)
	if err == nil {
		if chownErr := file.Chown(int(b.ExpectedUID), int(b.ExpectedGID)); chownErr != nil {
			file.Close()
			return chownErr
		}
		if chmodErr := file.Chmod(b.ExpectedMode); chmodErr != nil {
			file.Close()
			return chmodErr
		}
		if syncErr := file.Sync(); syncErr != nil {
			file.Close()
			return syncErr
		}
		if closeErr := file.Close(); closeErr != nil {
			return closeErr
		}
		dir, openErr := os.Open(filepath.Dir(b.TokenFile))
		if openErr != nil {
			return openErr
		}
		defer dir.Close()
		if syncErr := dir.Sync(); syncErr != nil {
			return syncErr
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	return b.validateTokenFile(true)
}
func (b *MicroK8sBackend) validateTokenFile(required bool) error {
	info, err := os.Lstat(b.TokenFile)
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 || stat.Uid != b.ExpectedUID || stat.Gid != b.ExpectedGID || info.Mode().Perm() != b.ExpectedMode.Perm() {
		return fmt.Errorf("MicroK8s token file is unsafe")
	}
	return nil
}
func validatePinnedMicroK8s() error {
	target, err := os.Readlink("/snap/microk8s/current")
	if err != nil || (target != "9072" && target != "9075" && target != "/snap/microk8s/9072" && target != "/snap/microk8s/9075") {
		return fmt.Errorf("MicroK8s revision is unsupported")
	}
	pins := map[string]string{
		"/snap/microk8s/current/scripts/wrappers/add_token.py": "3e3cc5b9a7b041595770635836992e36e19ca342f3b095a0bc0b3759ad915b8c",
		"/snap/microk8s/current/microk8s-add-node.wrapper":     "3a56a5bf4b6c2f10d6df299b4949b5c706928a68953f21c686772a874a78d717",
	}
	for path, want := range pins {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("pinned MicroK8s helper is unavailable")
		}
		sum := sha256.Sum256(data)
		if !hmac.Equal([]byte(hex.EncodeToString(sum[:])), []byte(want)) {
			return fmt.Errorf("pinned MicroK8s helper digest mismatch")
		}
	}
	return nil
}
func expireTokenInPlace(ctx context.Context, path string, uid, gid uint32, mode os.FileMode, token string, now func() time.Time, beforeWrite func(), writeExpiry func(*os.File, []byte, int64) (int, error), syncFile func(*os.File) error) error {
	file, unlock, err := openLockedTokenFile(ctx, path, uid, gid, mode)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := readLockedTokenFile(file)
	if err != nil {
		return err
	}
	_, count, epoch, offset, err := parseTokenFile(data, token)
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	if count != 1 || offset < 0 {
		return fmt.Errorf("broker token persistence is ambiguous")
	}
	clock := time.Now
	if now != nil {
		clock = now
	}
	if beforeWrite != nil {
		beforeWrite()
	}
	if writeExpiry == nil {
		writeExpiry = func(file *os.File, value []byte, offset int64) (int, error) { return file.WriteAt(value, offset) }
	}
	if epoch > clock().Unix() {
		n, writeErr := writeExpiry(file, []byte("0000000001"), offset)
		if writeErr != nil || n != 10 {
			return fmt.Errorf("expire MicroK8s token safely")
		}
	}
	if syncFile == nil {
		syncFile = func(file *os.File) error { return file.Sync() }
	}
	if err := syncFile(file); err != nil {
		return fmt.Errorf("persist MicroK8s token revocation: %w", err)
	}
	fresh, err := readLockedTokenFile(file)
	if err != nil || !sameTokenInode(path, file) {
		return fmt.Errorf("verify MicroK8s token revocation")
	}
	_, count, epoch, offset, err = parseTokenFile(fresh, token)
	if err != nil || count != 1 || offset < 0 || epoch > clock().Unix() {
		return fmt.Errorf("verify MicroK8s token revocation")
	}
	return nil
}

func openLockedTokenFile(ctx context.Context, path string, uid, gid uint32, mode os.FileMode) (*os.File, func(), error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != mode.Perm() || st.Uid != uid || st.Gid != gid || st.Nlink != 1 || !sameTokenInode(path, file) {
		file.Close()
		return nil, nil, fmt.Errorf("MicroK8s token file is unsafe")
	}
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			file.Close()
			return nil, nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return file, func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = file.Close() }, nil
}
func sameTokenInode(path string, file *os.File) bool {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return false
	}
	opened, err := file.Stat()
	return err == nil && os.SameFile(pathInfo, opened) && pathInfo.Mode()&os.ModeSymlink == 0
}
func readLockedTokenFile(file *os.File) ([]byte, error) {
	info, err := file.Stat()
	if err != nil || info.Size() > maxTokenFileBytes {
		return nil, fmt.Errorf("MicroK8s token file size is invalid")
	}
	data := make([]byte, info.Size())
	n, err := file.ReadAt(data, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return data[:n], nil
}
func parseTokenFile(data []byte, target string) ([]string, int, int64, int64, error) {
	if len(data) == 0 {
		return []string{}, 0, 0, -1, nil
	}
	if data[len(data)-1] != '\n' {
		return nil, 0, 0, -1, fmt.Errorf("MicroK8s token file is malformed")
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	count := 0
	var targetEpoch int64
	targetOffset := int64(-1)
	lineOffset := int64(0)
	for _, line := range lines {
		match := tokenLinePattern.FindStringSubmatch(line)
		if match == nil {
			return nil, 0, 0, -1, fmt.Errorf("MicroK8s token file is malformed")
		}
		var epoch int64
		if match[2] != "" {
			parsed, err := strconv.ParseInt(match[2], 10, 64)
			if err != nil || parsed < 1 || parsed > 4102444800 {
				return nil, 0, 0, -1, fmt.Errorf("MicroK8s token expiry is invalid")
			}
			epoch = parsed
		}
		if hmac.Equal([]byte(match[1]), []byte(target)) {
			count++
			targetEpoch = epoch
			if match[2] != "" {
				targetOffset = lineOffset + 33
			}
		}
		lineOffset += int64(len(line) + 1)
	}
	return lines, count, targetEpoch, targetOffset, nil
}
func validJoinURL(candidate, tokenCheck string) bool {
	u, err := url.Parse("https://" + candidate)
	if err != nil || u.User != nil || u.Scheme != "https" || u.RawQuery != "" || u.Fragment != "" || u.Path != "/"+tokenCheck {
		return false
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port >= 1 && port <= 65535
}
