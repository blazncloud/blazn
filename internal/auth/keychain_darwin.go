//go:build darwin

package auth

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	errSecSuccess       int32 = 0
	errSecDuplicateItem int32 = -25299
)

func stringPointer(value string) uintptr {
	if value == "" {
		return 0
	}
	return uintptr(unsafe.Pointer(unsafe.StringData(value)))
}

func bytesPointer(value []byte) uintptr {
	if len(value) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(unsafe.SliceData(value)))
}

func storeDarwinCredential(secret []byte) error {
	if len(secret) == 0 {
		return errors.New("refusing to store an empty Keychain credential")
	}
	security, err := purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return fmt.Errorf("open Security framework: %w", err)
	}
	defer purego.Dlclose(security)
	coreFoundation, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return fmt.Errorf("open CoreFoundation framework: %w", err)
	}
	defer purego.Dlclose(coreFoundation)

	var add func(uintptr, uint32, uintptr, uint32, uintptr, uint32, uintptr, *uintptr) int32
	var find func(uintptr, uint32, uintptr, uint32, uintptr, uintptr, uintptr, *uintptr) int32
	var modify func(uintptr, uintptr, uint32, uintptr) int32
	var open func(uintptr, *uintptr) int32
	var release func(uintptr)
	purego.RegisterLibFunc(&add, security, "SecKeychainAddGenericPassword")
	purego.RegisterLibFunc(&find, security, "SecKeychainFindGenericPassword")
	purego.RegisterLibFunc(&modify, security, "SecKeychainItemModifyAttributesAndData")
	purego.RegisterLibFunc(&open, security, "SecKeychainOpen")
	purego.RegisterLibFunc(&release, coreFoundation, "CFRelease")

	service := credentialService
	account := credentialAccount
	var keychain uintptr
	if path := os.Getenv("BLAZN_TEST_KEYCHAIN_PATH"); path != "" {
		if os.Getenv("BLAZN_ALLOW_TEST_KEYCHAIN") != "1" {
			return errors.New("BLAZN_TEST_KEYCHAIN_PATH requires BLAZN_ALLOW_TEST_KEYCHAIN=1")
		}
		pathBytes := append([]byte(path), 0)
		status := open(bytesPointer(pathBytes), &keychain)
		if status != errSecSuccess || keychain == 0 {
			return fmt.Errorf("open test Keychain failed with OSStatus %d", status)
		}
		defer release(keychain)
	}
	var item uintptr
	status := add(keychain, uint32(len(service)), stringPointer(service), uint32(len(account)), stringPointer(account), uint32(len(secret)), bytesPointer(secret), &item)
	if item != 0 {
		release(item)
	}
	if status == errSecSuccess {
		return nil
	}
	if status != errSecDuplicateItem {
		return fmt.Errorf("Keychain add failed with OSStatus %d", status)
	}

	item = 0
	status = find(keychain, uint32(len(service)), stringPointer(service), uint32(len(account)), stringPointer(account), 0, 0, &item)
	if status != errSecSuccess || item == 0 {
		return fmt.Errorf("Keychain lookup for update failed with OSStatus %d", status)
	}
	defer release(item)
	status = modify(item, 0, uint32(len(secret)), bytesPointer(secret))
	if status != errSecSuccess {
		return fmt.Errorf("Keychain update failed with OSStatus %d", status)
	}
	return nil
}
