//go:build linux

// Package systemdenv provides the opt-in Linux systemd user-manager
// environment publication boundary. It is deliberately not wired into the
// production command runtime: a native desktop-inheritance proof is required
// before session mode can be selected.
package systemdenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/activation"
	"github.com/blazncloud/blazn/internal/proxy/state"
	"github.com/godbus/dbus/v5"
)

const (
	publicationMechanism = "systemd_user_environment"
	managerName          = "org.freedesktop.systemd1"
	managerPath          = dbus.ObjectPath("/org/freedesktop/systemd1")
	managerInterface     = "org.freedesktop.systemd1.Manager"
	managerProperties    = "org.freedesktop.DBus.Properties"
	defaultTimeout       = 5 * time.Second
)

var (
	errTransport = errors.New("systemd user environment transport unavailable")
	digestRE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	uuidRE       = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	busIDRE      = regexp.MustCompile(`^[0-9a-f]{32}$`)
	ownerRE      = regexp.MustCompile(`^:[0-9]+\.[0-9]+$`)
)

type busRequest struct {
	Path string
	UID  int
}

// managerProof binds operations to one same-UID user bus and one unique
// systemd manager owner. DesktopInheritance is intentionally separate: the
// presence of a user manager alone does not prove that desktop applications
// inherit its environment.
type managerProof struct {
	OwnerUID           int
	BusID              string
	ManagerOwner       string
	DesktopInheritance bool
}

type transport interface {
	Probe(context.Context, busRequest) (managerProof, error)
	GetEnvironment(context.Context, busRequest) (managerProof, []string, error)
	SetEnvironment(context.Context, busRequest, []string) (managerProof, error)
	UnsetEnvironment(context.Context, busRequest, []string) (managerProof, error)
}

// Adapter implements activation.Environment and state.EnvironmentRestorer for
// the frozen five-variable Linux systemd user-manager boundary.
type Adapter struct {
	uid       int
	timeout   time.Duration
	transport transport

	mu     sync.Mutex
	proof  managerProof
	pinned bool
}

// New constructs the production transport without probing or mutating the
// user manager. Session activation remains unavailable until a separately
// reviewed desktop-inheritance capability is supplied.
func New() *Adapter {
	return &Adapter{uid: os.Getuid(), timeout: defaultTimeout, transport: dbusTransport{}}
}

func (a *Adapter) String() string   { return "[systemd user environment adapter]" }
func (a *Adapter) GoString() string { return a.String() }
func (a *Adapter) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.String())
}

func (*Adapter) Platform() string { return runtime.GOOS }

func (a *Adapter) ResolveMode(requested string) (string, string, error) {
	if requested != "auto" && requested != "session" {
		return "", "", activation.ErrSessionUnsupported
	}
	proof, err := a.probe(context.Background())
	if err != nil || !proof.DesktopInheritance {
		return "", "", activation.ErrSessionUnsupported
	}
	return "session", publicationMechanism, nil
}

func (a *Adapter) SessionIdentity(ctx context.Context) (string, error) {
	proof, err := a.probe(ctx)
	if err != nil {
		return "", unavailable(ctx)
	}
	return fmt.Sprintf("uid:%d/systemd-user:%s/%s", a.uid, proof.BusID, proof.ManagerOwner), nil
}

func (a *Adapter) Snapshot(ctx context.Context, names []string) (map[string]activation.PriorValue, error) {
	if !exactNames(names) {
		return nil, activation.ErrUnavailable
	}
	proof, entries, err := a.get(ctx)
	if err != nil || a.acceptProof(proof) != nil {
		return nil, unavailable(ctx)
	}
	values, err := parseEnvironment(entries)
	if err != nil {
		return nil, activation.ErrUnavailable
	}
	result := make(map[string]activation.PriorValue, len(state.EnvironmentNames))
	for _, name := range state.EnvironmentNames {
		value, present := values[name]
		result[name] = activation.PriorValue{Present: present, Value: value}
	}
	return result, nil
}

func (a *Adapter) Publish(ctx context.Context, mechanism string, values []activation.PublishedValue) error {
	if mechanism != publicationMechanism || !validPublished(values) || !a.capabilityReady() {
		return activation.ErrSessionUnsupported
	}
	for _, value := range values {
		proof, err := a.set(ctx, []string{value.Name + "=" + value.Value})
		if err != nil || a.acceptProof(proof) != nil {
			return unavailable(ctx)
		}
	}
	return nil
}

func (a *Adapter) BaseEnvironment() []string { return nil }

func (a *Adapter) CompareAndSet(ctx context.Context, request state.CompareAndSetRequest) (state.CompareAndSetResult, error) {
	if !validCASRequest(request) {
		return state.CASConflict, activation.ErrUnavailable
	}
	proof, entries, err := a.get(ctx)
	if err != nil || a.acceptProof(proof) != nil {
		return state.CASConflict, unavailable(ctx)
	}
	values, err := parseEnvironment(entries)
	if err != nil {
		return state.CASConflict, activation.ErrUnavailable
	}
	live, present := values[request.Name]
	if exactPrior(present, live, request) {
		return state.CASAlreadyRestored, nil
	}
	if !present || valueDigest(live) != request.ExpectedValueDigest {
		return state.CASConflict, nil
	}

	if request.PriorPresent {
		proof, err = a.set(ctx, []string{request.Name + "=" + *request.PriorValue})
	} else {
		proof, err = a.unset(ctx, []string{request.Name})
	}
	if err != nil || a.acceptProof(proof) != nil {
		return state.CASConflict, unavailable(ctx)
	}

	// Verify the exact absent/empty/value state after the direct manager call.
	// If another actor changes it, report conflict and leave that value alone.
	proof, entries, err = a.get(ctx)
	if err != nil || a.acceptProof(proof) != nil {
		return state.CASConflict, unavailable(ctx)
	}
	values, err = parseEnvironment(entries)
	if err != nil {
		return state.CASConflict, activation.ErrUnavailable
	}
	live, present = values[request.Name]
	if !exactPrior(present, live, request) {
		return state.CASConflict, nil
	}
	return state.CASRestored, nil
}

func (a *Adapter) probe(ctx context.Context) (managerProof, error) {
	if a == nil || a.transport == nil || a.uid < 0 {
		return managerProof{}, errTransport
	}
	callCtx, cancel := a.callContext(ctx)
	defer cancel()
	proof, err := a.transport.Probe(callCtx, a.request())
	if err != nil || callCtx.Err() != nil || a.acceptProof(proof) != nil {
		return managerProof{}, errTransport
	}
	return proof, nil
}

func (a *Adapter) get(ctx context.Context) (managerProof, []string, error) {
	callCtx, cancel := a.callContext(ctx)
	defer cancel()
	proof, values, err := a.transport.GetEnvironment(callCtx, a.request())
	if err != nil || callCtx.Err() != nil {
		return managerProof{}, nil, errTransport
	}
	return proof, values, nil
}

func (a *Adapter) set(ctx context.Context, values []string) (managerProof, error) {
	callCtx, cancel := a.callContext(ctx)
	defer cancel()
	proof, err := a.transport.SetEnvironment(callCtx, a.request(), append([]string(nil), values...))
	if err != nil || callCtx.Err() != nil {
		return managerProof{}, errTransport
	}
	return proof, nil
}

func (a *Adapter) unset(ctx context.Context, names []string) (managerProof, error) {
	callCtx, cancel := a.callContext(ctx)
	defer cancel()
	proof, err := a.transport.UnsetEnvironment(callCtx, a.request(), append([]string(nil), names...))
	if err != nil || callCtx.Err() != nil {
		return managerProof{}, errTransport
	}
	return proof, nil
}

func (a *Adapter) request() busRequest {
	return busRequest{Path: filepath.Join("/run/user", strconv.Itoa(a.uid), "bus"), UID: a.uid}
}

func (a *Adapter) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := a.timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (a *Adapter) acceptProof(proof managerProof) error {
	if a == nil || proof.OwnerUID != a.uid || !busIDRE.MatchString(proof.BusID) || !ownerRE.MatchString(proof.ManagerOwner) {
		return errTransport
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.pinned {
		a.proof, a.pinned = proof, true
		return nil
	}
	if a.proof.OwnerUID != proof.OwnerUID || a.proof.BusID != proof.BusID || a.proof.ManagerOwner != proof.ManagerOwner || a.proof.DesktopInheritance != proof.DesktopInheritance {
		return errTransport
	}
	return nil
}

func (a *Adapter) capabilityReady() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pinned && a.proof.DesktopInheritance
}

func unavailable(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return errors.Join(activation.ErrUnavailable, ctx.Err())
	}
	return activation.ErrUnavailable
}

func exactNames(names []string) bool {
	if len(names) != len(state.EnvironmentNames) {
		return false
	}
	for index, name := range state.EnvironmentNames {
		if names[index] != name {
			return false
		}
	}
	return true
}

func allowedName(name string) bool {
	for _, allowed := range state.EnvironmentNames {
		if name == allowed {
			return true
		}
	}
	return false
}

func parseEnvironment(entries []string) (map[string]string, error) {
	values := make(map[string]string, len(state.EnvironmentNames))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsAny(name, "\x00\n\r") || strings.ContainsAny(value, "\x00\n\r") {
			return nil, errTransport
		}
		if !allowedName(name) {
			continue
		}
		if _, duplicate := values[name]; duplicate {
			return nil, errTransport
		}
		values[name] = value
	}
	return values, nil
}

func validPublished(values []activation.PublishedValue) bool {
	if len(values) != len(state.EnvironmentNames) {
		return false
	}
	for index, name := range state.EnvironmentNames {
		value := values[index]
		if value.Name != name || strings.ContainsAny(value.Value, "\x00\n\r") || !validMarker(value.Marker, name) {
			return false
		}
	}
	return true
}

func validCASRequest(request state.CompareAndSetRequest) bool {
	if !allowedName(request.Name) || !digestRE.MatchString(request.ExpectedValueDigest) || !validMarker(request.ActivationMarker, request.Name) {
		return false
	}
	if request.PriorPresent != (request.PriorValue != nil) {
		return false
	}
	return request.PriorValue == nil || !strings.ContainsAny(*request.PriorValue, "\x00\n\r")
}

func validMarker(marker, name string) bool {
	activationID, markerName, ok := strings.Cut(marker, ":")
	return ok && uuidRE.MatchString(activationID) && markerName == name
}

func exactPrior(present bool, value string, request state.CompareAndSetRequest) bool {
	if present != request.PriorPresent {
		return false
	}
	return !present || value == *request.PriorValue
}

func valueDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type dbusTransport struct{}

func (dbusTransport) Probe(ctx context.Context, request busRequest) (managerProof, error) {
	proof, _, err := callManager(ctx, request, "", nil)
	// Direct user-manager discovery cannot prove that an already-running or
	// future desktop application inherits SetEnvironment. Keep this false until
	// the deferred native desktop fixture supplies stronger evidence.
	proof.DesktopInheritance = false
	return proof, err
}

func (dbusTransport) GetEnvironment(ctx context.Context, request busRequest) (managerProof, []string, error) {
	return callManager(ctx, request, managerProperties+".Get", []any{managerInterface, "Environment"})
}

func (dbusTransport) SetEnvironment(ctx context.Context, request busRequest, values []string) (managerProof, error) {
	proof, _, err := callManager(ctx, request, managerInterface+".SetEnvironment", []any{values})
	return proof, err
}

func (dbusTransport) UnsetEnvironment(ctx context.Context, request busRequest, names []string) (managerProof, error) {
	proof, _, err := callManager(ctx, request, managerInterface+".UnsetEnvironment", []any{names})
	return proof, err
}

func callManager(ctx context.Context, request busRequest, method string, arguments []any) (managerProof, []string, error) {
	if err := validateBusSocket(request.Path, request.UID); err != nil {
		return managerProof{}, nil, errTransport
	}
	raw, err := (&netDialer{}).dial(ctx, request.Path)
	if err != nil {
		return managerProof{}, nil, errTransport
	}
	defer raw.Close()
	peerUID, err := socketPeerUID(raw)
	if err != nil || peerUID != request.UID {
		return managerProof{}, nil, errTransport
	}
	connection, err := dbus.NewConn(raw, dbus.WithContext(ctx))
	if err != nil {
		return managerProof{}, nil, errTransport
	}
	defer connection.Close()
	if err := runStage(ctx, connection, func() error { return connection.Auth(nil) }); err != nil {
		return managerProof{}, nil, errTransport
	}
	if err := runStage(ctx, connection, connection.Hello); err != nil {
		return managerProof{}, nil, errTransport
	}
	var busID, owner string
	if err := connection.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetId", 0).Store(&busID); err != nil || !busIDRE.MatchString(busID) {
		return managerProof{}, nil, errTransport
	}
	if err := connection.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, managerName).Store(&owner); err != nil || !ownerRE.MatchString(owner) {
		return managerProof{}, nil, errTransport
	}
	proof := managerProof{OwnerUID: peerUID, BusID: busID, ManagerOwner: owner}
	if method == "" {
		return proof, nil, nil
	}
	call := connection.Object(owner, managerPath).CallWithContext(ctx, method, 0, arguments...)
	if call.Err != nil {
		return managerProof{}, nil, errTransport
	}
	if method == managerProperties+".Get" {
		var environment dbus.Variant
		if err := call.Store(&environment); err != nil {
			return managerProof{}, nil, errTransport
		}
		values, ok := environment.Value().([]string)
		if !ok {
			return managerProof{}, nil, errTransport
		}
		return proof, values, nil
	}
	return proof, nil, nil
}

// netDialer is a tiny seam kept private so the production implementation can
// only dial the validated Unix socket; tests exercise behavior through the
// higher-level injected transport.
type netDialer struct{}

func (*netDialer) dial(ctx context.Context, path string) (*net.UnixConn, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		_ = connection.Close()
		return nil, errTransport
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := unixConnection.SetDeadline(deadline); err != nil {
			_ = unixConnection.Close()
			return nil, err
		}
	}
	return unixConnection, nil
}

func validateBusSocket(path string, uid int) error {
	wantDirectory := filepath.Join("/run/user", strconv.Itoa(uid))
	if path != filepath.Join(wantDirectory, "bus") {
		return errTransport
	}
	directory, err := os.Lstat(wantDirectory)
	if err != nil || !directory.IsDir() || directory.Mode().Perm()&0o077 != 0 || fileUID(directory) != uid {
		return errTransport
	}
	socket, err := os.Lstat(path)
	if err != nil || socket.Mode()&os.ModeSocket == 0 || fileUID(socket) != uid {
		return errTransport
	}
	return nil
}

func fileUID(info os.FileInfo) int {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}

func socketPeerUID(connection *net.UnixConn) (int, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return -1, err
	}
	uid := -1
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, currentErr := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if currentErr != nil {
			socketErr = currentErr
			return
		}
		uid = int(credential.Uid)
	}); err != nil {
		return -1, err
	}
	return uid, socketErr
}

func runStage(ctx context.Context, connection *dbus.Conn, stage func() error) error {
	result := make(chan error, 1)
	go func() { result <- stage() }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = connection.Close()
		return ctx.Err()
	}
}
