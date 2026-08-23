//go:build linux

package credential

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	secretServiceName      = "org.freedesktop.secrets"
	secretServicePath      = dbus.ObjectPath("/org/freedesktop/secrets")
	secretServiceInterface = "org.freedesktop.Secret.Service"
	secretSessionInterface = "org.freedesktop.Secret.Session"
	secretItemInterface    = "org.freedesktop.Secret.Item"
	secretProperties       = "org.freedesktop.DBus.Properties"
	secretNamespace        = "com.blazn.proxy.destination.v1alpha1"
	secretLookupTimeout    = 5 * time.Second
)

var errSecretServiceTransport = errors.New("secret service transport unavailable")

type secretServiceRequest struct {
	BusPath string
	UID     int
	Service string
	Account string
}

type secretServiceItem struct {
	Path       string
	Session    string
	Locked     bool
	Attributes map[string]string
	Value      []byte
}

type secretServiceResponse struct {
	OwnerUID int
	Session  string
	Items    []secretServiceItem
}

type secretServiceTransport interface {
	Lookup(context.Context, secretServiceRequest) (secretServiceResponse, error)
}

// SecretServiceBackend reads destination credentials from the calling Linux
// user's Secret Service. It has no file, command, environment, or fallback
// backend and exposes no credential mutation operation.
type SecretServiceBackend struct {
	uid       int
	timeout   time.Duration
	transport secretServiceTransport
}

// NewSecretServiceBackend constructs the production, read-only Linux backend.
// The session bus is always /run/user/<OS uid>/bus; environment variables are
// deliberately not consulted.
func NewSecretServiceBackend() *SecretServiceBackend {
	return &SecretServiceBackend{uid: os.Getuid(), timeout: secretLookupTimeout, transport: dbusSecretServiceTransport{}}
}

func (b *SecretServiceBackend) String() string   { return "[REDACTED credential backend]" }
func (b *SecretServiceBackend) GoString() string { return "[REDACTED credential backend]" }
func (b *SecretServiceBackend) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED credential backend]"`), nil
}

func (b *SecretServiceBackend) Lookup(ctx context.Context, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, unavailable(FailureCancelled, err)
	}
	if b == nil || b.transport == nil || b.uid < 0 || len(ref) == 0 || len(ref) > maxReferenceBytes || !canonicalRefRE.MatchString(ref) {
		return nil, unavailable(FailureBackendUnavailable, nil)
	}
	timeout := b.timeout
	if timeout <= 0 {
		timeout = secretLookupTimeout
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request := secretServiceRequest{
		BusPath: filepath.Join("/run/user", strconv.Itoa(b.uid), "bus"),
		UID:     b.uid,
		Service: secretNamespace,
		Account: ref,
	}
	response, err := b.transport.Lookup(lookupCtx, request)
	if err != nil {
		zeroSecretServiceItems(response.Items)
		if lookupCtx.Err() != nil {
			return nil, unavailable(FailureCancelled, lookupCtx.Err())
		}
		return nil, unavailable(FailureBackendUnavailable, nil)
	}
	defer zeroSecretServiceItems(response.Items)
	if response.OwnerUID != b.uid || !validSecretSession(response.Session) || len(response.Items) != 1 {
		return nil, unavailable(FailureBackendUnavailable, nil)
	}
	item := response.Items[0]
	if item.Locked || item.Session != response.Session || !validSecretItemPath(item.Path) ||
		len(item.Attributes) != 2 || item.Attributes["service"] != secretNamespace || item.Attributes["account"] != ref {
		return nil, unavailable(FailureBackendUnavailable, nil)
	}
	if err := validateValue(item.Value); err != nil {
		return nil, err
	}
	return append([]byte(nil), item.Value...), nil
}

func validSecretSession(value string) bool {
	return dbus.ObjectPath(value).IsValid() && strings.HasPrefix(value, "/org/freedesktop/secrets/session/")
}

func validSecretItemPath(value string) bool {
	return dbus.ObjectPath(value).IsValid() && strings.HasPrefix(value, "/org/freedesktop/secrets/collection/")
}

func zeroSecretServiceItems(items []secretServiceItem) {
	for index := range items {
		zero(items[index].Value)
	}
}

type dbusSecretServiceTransport struct{}

type secretValue struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func (dbusSecretServiceTransport) Lookup(ctx context.Context, request secretServiceRequest) (secretServiceResponse, error) {
	if err := validateBusSocket(request.BusPath, request.UID); err != nil {
		return secretServiceResponse{}, errSecretServiceTransport
	}
	unixConnection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: request.BusPath, Net: "unix"})
	if err != nil {
		return secretServiceResponse{}, errSecretServiceTransport
	}
	peerUID, err := unixPeerUID(unixConnection)
	if err != nil || peerUID != request.UID {
		_ = unixConnection.Close()
		return secretServiceResponse{}, errSecretServiceTransport
	}
	connection, err := dbus.NewConn(unixConnection)
	if err != nil {
		_ = unixConnection.Close()
		return secretServiceResponse{}, errSecretServiceTransport
	}
	defer connection.Close()
	if err := connection.Auth(nil); err != nil {
		return secretServiceResponse{}, errSecretServiceTransport
	}
	if err := connection.Hello(); err != nil {
		return secretServiceResponse{}, errSecretServiceTransport
	}

	var owner string
	if err := connection.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, secretServiceName).Store(&owner); err != nil || !strings.HasPrefix(owner, ":") {
		return secretServiceResponse{}, errSecretServiceTransport
	}
	// Pin every subsequent call to the validated unique owner. A replacement
	// that acquires the well-known name mid-lookup must not inherit authority.
	service := connection.Object(owner, secretServicePath)
	var sessionOutput dbus.Variant
	var sessionPath dbus.ObjectPath
	if err := service.CallWithContext(ctx, secretServiceInterface+".OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&sessionOutput, &sessionPath); err != nil || sessionOutput.Value() != "" || !validSecretSession(string(sessionPath)) {
		return secretServiceResponse{}, errSecretServiceTransport
	}
	defer connection.Object(owner, sessionPath).CallWithContext(context.Background(), secretSessionInterface+".Close", dbus.FlagNoReplyExpected)

	attributes := map[string]string{"service": request.Service, "account": request.Account}
	var unlocked, locked []dbus.ObjectPath
	if err := service.CallWithContext(ctx, secretServiceInterface+".SearchItems", 0, attributes).Store(&unlocked, &locked); err != nil {
		return secretServiceResponse{}, errSecretServiceTransport
	}
	response := secretServiceResponse{OwnerUID: peerUID, Session: string(sessionPath), Items: make([]secretServiceItem, 0, len(unlocked)+len(locked))}
	seen := make(map[dbus.ObjectPath]struct{}, len(unlocked)+len(locked))
	for _, path := range locked {
		if _, duplicate := seen[path]; duplicate || !validSecretItemPath(string(path)) {
			zeroSecretServiceItems(response.Items)
			return secretServiceResponse{}, errSecretServiceTransport
		}
		seen[path] = struct{}{}
		response.Items = append(response.Items, secretServiceItem{Path: string(path), Session: string(sessionPath), Locked: true, Attributes: attributes})
	}
	for _, path := range unlocked {
		if _, duplicate := seen[path]; duplicate || !validSecretItemPath(string(path)) {
			zeroSecretServiceItems(response.Items)
			return secretServiceResponse{}, errSecretServiceTransport
		}
		seen[path] = struct{}{}
		object := connection.Object(owner, path)
		var attributeVariant dbus.Variant
		if err := object.CallWithContext(ctx, secretProperties+".Get", 0, secretItemInterface, "Attributes").Store(&attributeVariant); err != nil {
			zeroSecretServiceItems(response.Items)
			return secretServiceResponse{}, errSecretServiceTransport
		}
		itemAttributes, ok := attributeVariant.Value().(map[string]string)
		if !ok || len(itemAttributes) != 2 || itemAttributes["service"] != request.Service || itemAttributes["account"] != request.Account {
			zeroSecretServiceItems(response.Items)
			return secretServiceResponse{}, errSecretServiceTransport
		}
		var secret secretValue
		if err := object.CallWithContext(ctx, secretItemInterface+".GetSecret", 0, sessionPath).Store(&secret); err != nil {
			zero(secret.Value)
			zeroSecretServiceItems(response.Items)
			return secretServiceResponse{}, errSecretServiceTransport
		}
		if secret.Session != sessionPath || len(secret.Parameters) != 0 || (secret.ContentType != "" && secret.ContentType != "text/plain") {
			zero(secret.Value)
			zeroSecretServiceItems(response.Items)
			return secretServiceResponse{}, errSecretServiceTransport
		}
		response.Items = append(response.Items, secretServiceItem{Path: string(path), Session: string(secret.Session), Attributes: itemAttributes, Value: secret.Value})
	}
	return response, nil
}

func validateBusSocket(path string, uid int) error {
	want := filepath.Join("/run/user", strconv.Itoa(uid), "bus")
	if path != want {
		return errSecretServiceTransport
	}
	directory, err := os.Lstat(filepath.Dir(path))
	if err != nil || !directory.IsDir() || directory.Mode().Perm()&0o077 != 0 || fileUID(directory) != uid {
		return errSecretServiceTransport
	}
	socket, err := os.Lstat(path)
	if err != nil || socket.Mode()&os.ModeSocket == 0 || fileUID(socket) != uid {
		return errSecretServiceTransport
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

func unixPeerUID(connection *net.UnixConn) (int, error) {
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

func (request secretServiceRequest) String() string {
	return fmt.Sprintf("secret service request uid=%d service=%s account=[REDACTED]", request.UID, request.Service)
}

func (request secretServiceRequest) GoString() string { return request.String() }
func (request secretServiceRequest) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED secret service request]"`), nil
}

func (response secretServiceResponse) String() string {
	return fmt.Sprintf("secret service response owner_uid=%d session=[REDACTED] items=%d", response.OwnerUID, len(response.Items))
}

func (response secretServiceResponse) GoString() string { return response.String() }
func (response secretServiceResponse) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED secret service response]"`), nil
}

func (item secretServiceItem) String() string   { return "[REDACTED secret service item]" }
func (item secretServiceItem) GoString() string { return item.String() }
func (item secretServiceItem) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED secret service item]"`), nil
}
