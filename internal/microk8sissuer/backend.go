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
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const maxCommandOutput = 16 << 10

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
	AddNodePath, TokenFile string
	ExpectedUID            uint32
	ExpectedGID            uint32
	ExpectedMode           os.FileMode
	Runner                 CommandRunner
	Now                    func() time.Time
	allowTestPaths         bool
}
type upstreamResponse struct {
	Token string   `json:"token"`
	URLs  []string `json:"urls"`
}

func (b *MicroK8sBackend) Healthy(ctx context.Context) error {
	if !b.allowTestPaths && (b.AddNodePath != "/snap/bin/microk8s.add-node" || b.TokenFile != "/var/snap/microk8s/current/credentials/cluster-tokens.txt") {
		return fmt.Errorf("MicroK8s paths are not allowlisted")
	}
	if !b.allowTestPaths {
		if err := validatePinnedMicroK8s(); err != nil {
			return err
		}
	}
	return b.validateTokenFile(false)
}
func (b *MicroK8sBackend) Issue(ctx context.Context, token string, ttl int) (BackendIssue, error) {
	if err := b.Healthy(ctx); err != nil {
		return BackendIssue{}, err
	}
	if err := b.ensureTokenFile(); err != nil {
		return BackendIssue{}, err
	}
	if !tokenLineAbsentOrExact(b.TokenFile, token) {
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
	if tokenLineCount(b.TokenFile, token) != 1 {
		return BackendIssue{}, fmt.Errorf("MicroK8s token persistence is ambiguous")
	}
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	return BackendIssue{TokenCheck: parsed.Token, URLs: parsed.URLs, ExpiresAt: now().UTC().Add(time.Duration(ttl) * time.Second)}, nil
}
func (b *MicroK8sBackend) Revoke(ctx context.Context, token string) error {
	if _, err := os.Lstat(b.TokenFile); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := b.Healthy(ctx); err != nil {
		return err
	}
	return rewriteTokenFile(b.TokenFile, b.ExpectedUID, b.ExpectedGID, b.ExpectedMode, token)
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
	if b.Runner == nil {
		return fmt.Errorf("MicroK8s runner is missing")
	}
	return nil
}
func tokenLineAbsentOrExact(path, token string) bool {
	count := tokenLineCount(path, token)
	return count >= 0 && count <= 1
}
func tokenLineCount(path, token string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if strings.HasPrefix(line, token) {
			if line != token && !strings.HasPrefix(line, token+"|") {
				return 2
			}
			count++
		}
	}
	return count
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
func rewriteTokenFile(path string, uid, gid uint32, mode os.FileMode, token string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || st.Nlink != 1 || st.Uid != uid || st.Gid != gid || info.Mode().Perm() != mode.Perm() {
		return fmt.Errorf("MicroK8s token file is unsafe")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if line == token || strings.HasPrefix(line, token+"|") {
			continue
		}
		kept = append(kept, line)
	}
	next := strings.Join(kept, "\n")
	if next == string(data) {
		return nil
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cluster-tokens-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(info.Mode().Perm()); err == nil {
		err = tmp.Chown(int(st.Uid), int(st.Gid))
	}
	if err == nil {
		_, err = tmp.WriteString(next)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
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
