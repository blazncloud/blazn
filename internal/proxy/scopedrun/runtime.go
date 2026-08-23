//go:build darwin || linux

// Package scopedrun wires the production, process-lifetime proxy run adapter.
// It deliberately exposes no durable listener or session publication surface.
package scopedrun

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/activation"
	"github.com/blazncloud/blazn/internal/proxy/credential"
	"github.com/blazncloud/blazn/internal/proxy/router"
	"github.com/blazncloud/blazn/internal/proxy/state"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

const childCancelGrace = 5 * time.Second

type serviceRunner interface {
	Run(context.Context, string, []string) (activation.Result, error)
}

type dependencies struct {
	loadPolicy activation.PolicyLoader
	resolve    credential.SnapshotResolver
	newService func(string, proxycontract.Policy, string, *credential.Snapshot) (serviceRunner, error)
	now        func() time.Time
}

// Commands implements the frozen proxy command surface, but delegates only
// scoped Run. Durable/session commands remain unavailable until their native
// listener, control, and environment-publication proofs exist.
type Commands struct{ deps dependencies }

// NewProduction is side-effect free. Policy and every destination credential
// are preflighted by Run before the OS-account state root or listener exists.
func NewProduction(stdin io.Reader, stdout, stderr io.Writer) (*Commands, error) {
	backend := productionCredentialBackend()
	resolver := &credential.Resolver{NodeRoute: backend, WorkspaceVault: backend}
	streams := processStreams{stdin: stdin, stdout: stdout, stderr: stderr}
	commands := &Commands{deps: dependencies{
		loadPolicy: activation.PolicyLoaderFunc(router.LoadPolicy),
		resolve:    resolver,
		now:        time.Now,
	}}
	commands.deps.newService = func(path string, policy proxycontract.Policy, digest string, snapshot *credential.Snapshot) (serviceRunner, error) {
		return newProductionService(path, policy, digest, snapshot, streams)
	}
	return commands, nil
}

func (c *Commands) Run(ctx context.Context, policyPath string, argv []string) (activation.Result, error) {
	if len(argv) == 0 || argv[0] == "" {
		return c.result("proxy run", "failed", 2), errors.New("proxy run requires an exact command argv")
	}
	policy, digest, err := c.deps.loadPolicy.Load(policyPath)
	if err != nil {
		return c.result("proxy run", "failed", 2), err
	}
	snapshot, err := c.deps.resolve.Resolve(ctx, policy)
	if err != nil {
		return c.result("proxy run", "failed", 3), errors.Join(activation.ErrCredentialUnavailable, err)
	}
	service, err := c.deps.newService(filepath.Clean(policyPath), policy, digest, snapshot)
	if err != nil {
		return c.result("proxy run", "failed", 7), errors.Join(activation.ErrUnavailable, err)
	}
	return service.Run(ctx, policyPath, append([]string(nil), argv...))
}

func (c *Commands) On(context.Context, string, string) (activation.Result, error) {
	return c.result("proxy on", "unsupported", 2), activation.ErrSessionUnsupported
}
func (c *Commands) Off(context.Context, bool) (activation.Result, error) {
	return c.result("proxy off", "unsupported", 7), activation.ErrUnavailable
}
func (c *Commands) Status(context.Context) (activation.Result, error) {
	return c.result("proxy status", "unsupported", 7), activation.ErrUnavailable
}
func (c *Commands) Doctor(context.Context, string) (activation.Result, error) {
	return c.result("proxy doctor", "unsupported", 7), activation.ErrUnavailable
}
func (c *Commands) Routes(context.Context, string) ([]activation.Route, error) {
	return nil, activation.ErrUnavailable
}
func (c *Commands) Tail(context.Context, string, bool) ([]activation.Event, error) {
	return nil, activation.ErrUnavailable
}
func (c *Commands) Reset(context.Context, bool, bool) (activation.Result, error) {
	return c.result("proxy reset", "unsupported", 7), activation.ErrUnavailable
}

func (c *Commands) result(command, status string, exit int) activation.Result {
	now := time.Now
	if c != nil && c.deps.now != nil {
		now = c.deps.now
	}
	return activation.Result{Command: command, ContractVersion: activation.ContractVersion, Status: status, State: "inactive", Timestamp: now().UTC().Format(time.RFC3339), ExitCode: exit}
}

type frozenPolicy struct {
	path   string
	policy proxycontract.Policy
	digest string
}

func (p frozenPolicy) Load(path string) (proxycontract.Policy, string, error) {
	if filepath.Clean(path) != p.path {
		return proxycontract.Policy{}, "", activation.ErrPolicyInvalid
	}
	return p.policy, p.digest, nil
}

func newProductionService(policyPath string, policy proxycontract.Policy, digest string, snapshot *credential.Snapshot, streams processStreams) (*activation.Service, error) {
	if snapshot == nil {
		return nil, activation.ErrCredentialUnavailable
	}
	store, err := state.NewForCurrentAccount()
	if err != nil {
		return nil, err
	}
	binary, process, err := currentProcessIdentity()
	if err != nil {
		return nil, err
	}
	environment := processEnvironment{uid: os.Getuid(), environ: os.Environ}
	listenerFactory := activation.EmbeddedListenerFactory{
		Address:            "127.0.0.1",
		Router:             router.Config{Resolver: router.EndpointResolver{}},
		Identity:           process,
		CredentialResolver: fixedSnapshot{policyDigest: digest, snapshot: snapshot},
	}
	return activation.New(activation.Dependencies{
		Store: activation.PersistentStore{Value: store}, Environment: environment,
		Listeners: listenerFactory, Controller: unavailableController{},
		Policies: frozenPolicy{path: filepath.Clean(policyPath), policy: policy, digest: digest},
		Runner:   ExecRunner{Streams: streams}, Binary: binary, OwnerUID: os.Getuid(), Now: time.Now,
	})
}

type fixedSnapshot struct {
	policyDigest string
	snapshot     *credential.Snapshot
}

func (f fixedSnapshot) Resolve(_ context.Context, policy proxycontract.Policy) (*credential.Snapshot, error) {
	digest, err := proxycontract.ContractDigest(policy)
	if err != nil || digest != f.policyDigest || f.snapshot == nil {
		return nil, activation.ErrPolicyInvalid
	}
	return f.snapshot, nil
}

type processEnvironment struct {
	uid     int
	environ func() []string
}

func (e processEnvironment) Platform() string { return runtime.GOOS }
func (e processEnvironment) SessionIdentity(context.Context) (string, error) {
	if e.uid < 0 || e.uid != os.Getuid() {
		return "", activation.ErrUnavailable
	}
	return fmt.Sprintf("uid:%d/process:%d", e.uid, os.Getpid()), nil
}
func (processEnvironment) ResolveMode(string) (string, string, error) {
	return "", "", activation.ErrSessionUnsupported
}
func (e processEnvironment) Snapshot(_ context.Context, names []string) (map[string]activation.PriorValue, error) {
	values := make(map[string]activation.PriorValue, len(names))
	for _, name := range names {
		value, present := lookupEnvironment(e.environ(), name)
		values[name] = activation.PriorValue{Present: present, Value: value}
	}
	return values, nil
}
func (processEnvironment) Publish(context.Context, string, []activation.PublishedValue) error {
	return activation.ErrSessionUnsupported
}
func (e processEnvironment) BaseEnvironment() []string {
	return childBaseEnvironment(e.environ())
}
func (processEnvironment) CompareAndSet(_ context.Context, request state.CompareAndSetRequest) (state.CompareAndSetResult, error) {
	for _, name := range state.EnvironmentNames {
		if request.Name == name {
			return state.CASAlreadyRestored, nil
		}
	}
	return state.CASConflict, errors.New("process environment restore target is outside the proxy contract")
}

func lookupEnvironment(environment []string, wanted string) (string, bool) {
	for index := len(environment) - 1; index >= 0; index-- {
		name, value, present := strings.Cut(environment[index], "=")
		if present && name == wanted {
			return value, true
		}
	}
	return "", false
}

func childBaseEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, present := strings.Cut(entry, "=")
		if !present || isProxyVariable(name) || isManagementCredential(name) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func isProxyVariable(name string) bool {
	for _, candidate := range state.EnvironmentNames {
		if name == candidate {
			return true
		}
	}
	return false
}

func isManagementCredential(name string) bool {
	if !strings.HasPrefix(name, "BLAZN_") {
		return false
	}
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "COOKIE"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

type processIdentity struct {
	pid        int
	start      string
	executable string
}

func (p processIdentity) Proof(_ context.Context, address, fingerprint string, metadata activation.ListenerMetadata) (state.ListenerIdentity, state.LiveListenerProof, error) {
	if p.pid != os.Getpid() || metadata.OwnerUID != int64(os.Getuid()) || metadata.Mode != "scoped_run" {
		return state.ListenerIdentity{}, state.LiveListenerProof{}, activation.ErrUnavailable
	}
	identity := state.ListenerIdentity{PID: p.pid, ProcessStartIdentity: p.start, ExecutableIdentity: p.executable, Address: address, ListenerKeyFingerprint: fingerprint}
	proof := state.LiveListenerProof{PID: p.pid, ProcessStartIdentity: p.start, ExecutableIdentity: p.executable, BinaryDigest: metadata.BinaryDigest, ListenerKeyFingerprint: fingerprint, ActivationNonce: metadata.Nonce, OwnerUID: int(metadata.OwnerUID), Generation: metadata.Generation, Mode: metadata.Mode, SessionIdentity: metadata.SessionIdentity}
	return identity, proof, nil
}

func currentProcessIdentity() (state.BinaryIdentity, processIdentity, error) {
	path, err := os.Executable()
	if err != nil {
		return state.BinaryIdentity{}, processIdentity{}, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(path) {
		return state.BinaryIdentity{}, processIdentity{}, errors.Join(activation.ErrUnavailable, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return state.BinaryIdentity{}, processIdentity{}, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		return state.BinaryIdentity{}, processIdentity{}, errors.Join(activation.ErrUnavailable, err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return state.BinaryIdentity{}, processIdentity{}, err
	}
	pathInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(openedInfo, pathInfo) {
		return state.BinaryIdentity{}, processIdentity{}, errors.Join(activation.ErrUnavailable, err)
	}
	stat, ok := openedInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return state.BinaryIdentity{}, processIdentity{}, activation.ErrUnavailable
	}
	var epoch [16]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		return state.BinaryIdentity{}, processIdentity{}, err
	}
	executable := fmt.Sprintf("dev:%d/inode:%d", stat.Dev, stat.Ino)
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	return state.BinaryIdentity{Path: path, Digest: digest}, processIdentity{pid: os.Getpid(), start: "epoch:" + hex.EncodeToString(epoch[:]), executable: executable}, nil
}

type unavailableController struct{}

func (unavailableController) Inspect(context.Context, int) (state.LiveListenerProof, bool, error) {
	return state.LiveListenerProof{}, false, activation.ErrUnavailable
}
func (unavailableController) Stop(context.Context, state.LiveListenerProof) error {
	return activation.ErrUnavailable
}

type processStreams struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// ExecRunner invokes exact argv through exec.CommandContext, never a shell.
// Only the supplied child environment and streams cross the process boundary.
type ExecRunner struct{ Streams processStreams }

func (r ExecRunner) Run(ctx context.Context, argv, environment []string) (int, error) {
	if len(argv) == 0 || argv[0] == "" {
		return 1, errors.New("child argv is empty")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = append([]string(nil), environment...)
	command.Stdin, command.Stdout, command.Stderr = r.Streams.stdin, r.Streams.stdout, r.Streams.stderr
	command.WaitDelay = childCancelGrace
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return command.Process.Signal(syscall.SIGTERM)
	}
	if err := command.Start(); err != nil {
		return 1, err
	}
	forward := make(chan os.Signal, 4)
	signal.Notify(forward, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for next := range forward {
			if command.Process != nil {
				_ = command.Process.Signal(next)
			}
		}
	}()
	waitErr := command.Wait()
	signal.Stop(forward)
	close(forward)
	<-done
	if waitErr == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal()), nil
			}
			return status.ExitStatus(), nil
		}
		return exit.ExitCode(), nil
	}
	return 1, waitErr
}
