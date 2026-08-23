//go:build darwin

package credential

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	proxyKeychainService = "com.blazn.proxy.destination.v1alpha1"

	errSecSuccess               int32 = 0
	errSecUserCanceled          int32 = -128
	errSecAuthFailed            int32 = -25293
	errSecItemNotFound          int32 = -25300
	errSecInteractionNotAllowed int32 = -25308
	errSecInteractionRequired   int32 = -25315
)

var errKeychainValueBounds = errors.New("Keychain value is outside the allowed bounds")

// KeychainFailureKind permits callers to distinguish operator-remediable
// Keychain failures without exposing a destination reference or native error.
type KeychainFailureKind string

const (
	KeychainNotFound       KeychainFailureKind = "not_found"
	KeychainLockedOrDenied KeychainFailureKind = "locked_or_denied"
	KeychainBackendError   KeychainFailureKind = "backend_error"
	KeychainInvalidInput   KeychainFailureKind = "invalid_input"
	KeychainInvalidValue   KeychainFailureKind = "invalid_value"
	KeychainCancelled      KeychainFailureKind = "cancelled"
)

// KeychainError is deliberately data-free apart from its coarse failure kind.
// It is safe to format or serialize in operational output.
type KeychainError struct {
	Kind KeychainFailureKind `json:"-"`
}

func (*KeychainError) Error() string    { return "proxy destination credential Keychain lookup failed" }
func (*KeychainError) String() string   { return "proxy destination credential Keychain lookup failed" }
func (*KeychainError) GoString() string { return "[REDACTED proxy destination Keychain error]" }
func (*KeychainError) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED proxy destination Keychain error]")
}

type darwinKeychainTransport interface {
	Lookup(context.Context, string, string) ([]byte, int32, error)
}

// DarwinKeychainBackend reads proxy destination credentials from the user's
// default login Keychain. It has no write, delete, or path-selection surface.
type DarwinKeychainBackend struct {
	transport darwinKeychainTransport
}

var _ Backend = (*DarwinKeychainBackend)(nil)

// NewDarwinKeychainBackend returns the production read-only Keychain backend.
// Production always uses the default Keychain and cannot be redirected by the
// environment.
func NewDarwinKeychainBackend() Backend {
	return &DarwinKeychainBackend{transport: nativeDarwinKeychainTransport{}}
}

func newDarwinKeychainBackend(transport darwinKeychainTransport) *DarwinKeychainBackend {
	return &DarwinKeychainBackend{transport: transport}
}

func (b *DarwinKeychainBackend) Lookup(ctx context.Context, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, keychainError(KeychainCancelled)
	}
	if b == nil || b.transport == nil {
		return nil, keychainError(KeychainBackendError)
	}
	if !isCanonicalReference(ref) {
		return nil, keychainError(KeychainInvalidInput)
	}

	raw, status, err := b.transport.Lookup(ctx, proxyKeychainService, ref)
	if err != nil {
		zero(raw)
		if ctx.Err() != nil {
			return nil, keychainError(KeychainCancelled)
		}
		if errors.Is(err, errKeychainValueBounds) {
			return nil, keychainError(KeychainInvalidValue)
		}
		return nil, keychainError(KeychainBackendError)
	}
	if ctx.Err() != nil {
		zero(raw)
		return nil, keychainError(KeychainCancelled)
	}
	if status != errSecSuccess {
		zero(raw)
		switch status {
		case errSecItemNotFound:
			return nil, keychainError(KeychainNotFound)
		case errSecUserCanceled:
			return nil, keychainError(KeychainCancelled)
		case errSecAuthFailed, errSecInteractionNotAllowed, errSecInteractionRequired:
			return nil, keychainError(KeychainLockedOrDenied)
		default:
			return nil, keychainError(KeychainBackendError)
		}
	}
	if err := validateValue(raw); err != nil {
		zero(raw)
		return nil, keychainError(KeychainInvalidValue)
	}
	value := append([]byte(nil), raw...)
	zero(raw)
	return value, nil
}

func keychainError(kind KeychainFailureKind) error { return &KeychainError{Kind: kind} }

type nativeDarwinKeychainTransport struct{}

type darwinKeychainNative struct {
	copyDefault func(*uintptr) int32
	find        func(uintptr, uint32, uintptr, uint32, uintptr, *uint32, *unsafe.Pointer, *uintptr) int32
	freeContent func(uintptr, unsafe.Pointer) int32
	release     func(uintptr)
}

func (nativeDarwinKeychainTransport) Lookup(ctx context.Context, service, account string) ([]byte, int32, error) {
	if err := ctx.Err(); err != nil {
		return nil, errSecSuccess, err
	}
	security, err := purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, errSecSuccess, errors.New("open Security framework")
	}
	defer purego.Dlclose(security)
	coreFoundation, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, errSecSuccess, errors.New("open CoreFoundation framework")
	}
	defer purego.Dlclose(coreFoundation)

	native := darwinKeychainNative{}
	purego.RegisterLibFunc(&native.copyDefault, security, "SecKeychainCopyDefault")
	purego.RegisterLibFunc(&native.find, security, "SecKeychainFindGenericPassword")
	purego.RegisterLibFunc(&native.freeContent, security, "SecKeychainItemFreeContent")
	purego.RegisterLibFunc(&native.release, coreFoundation, "CFRelease")
	return lookupDarwinKeychain(ctx, service, account, native)
}

func lookupDarwinKeychain(ctx context.Context, service, account string, native darwinKeychainNative) ([]byte, int32, error) {
	var keychain uintptr
	status := native.copyDefault(&keychain)
	if keychain != 0 {
		defer native.release(keychain)
	}
	if status != errSecSuccess {
		return nil, status, nil
	}
	if keychain == 0 {
		return nil, errSecSuccess, errors.New("resolve default Keychain")
	}

	var length uint32
	var data unsafe.Pointer
	var item uintptr
	status = native.find(keychain, uint32(len(service)), darwinStringPointer(service), uint32(len(account)), darwinStringPointer(account), &length, &data, &item)
	runtime.KeepAlive(service)
	runtime.KeepAlive(account)
	if item != 0 {
		defer native.release(item)
	}
	if data != nil {
		defer func() {
			scrubNativeKeychainValue(data, length)
			native.freeContent(0, data)
		}()
	}
	if status != errSecSuccess {
		return nil, status, nil
	}
	if data == nil || length == 0 {
		return nil, status, nil
	}
	if uint64(length) > uint64(MaxCredentialBytes) {
		return nil, status, errKeychainValueBounds
	}
	if err := ctx.Err(); err != nil {
		return nil, status, err
	}
	value := append([]byte(nil), unsafe.Slice((*byte)(data), int(length))...)
	return value, status, nil
}

func scrubNativeKeychainValue(data unsafe.Pointer, length uint32) {
	if data == nil || length == 0 {
		return
	}
	zero(unsafe.Slice((*byte)(data), int(length)))
	runtime.KeepAlive(data)
}

func darwinStringPointer(value string) uintptr {
	if value == "" {
		return 0
	}
	return uintptr(unsafe.Pointer(unsafe.StringData(value)))
}
