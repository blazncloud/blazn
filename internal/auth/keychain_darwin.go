//go:build darwin

package auth

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	errSecSuccess       int32 = 0
	errSecDuplicateItem int32 = -25299
	errSecItemNotFound  int32 = -25300
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

func storeDarwinCredential(account string, secret []byte) error {
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
	var keychain uintptr
	path, err := selectedDarwinKeychainPath()
	if err != nil {
		return err
	}
	if path != "" {
		pathBytes := append([]byte(path), 0)
		status := open(bytesPointer(pathBytes), &keychain)
		runtime.KeepAlive(pathBytes)
		if status != errSecSuccess || keychain == 0 {
			return fmt.Errorf("open test Keychain failed with OSStatus %d", status)
		}
		defer release(keychain)
	}
	var item uintptr
	status := add(keychain, uint32(len(service)), stringPointer(service), uint32(len(account)), stringPointer(account), uint32(len(secret)), bytesPointer(secret), &item)
	runtime.KeepAlive(service)
	runtime.KeepAlive(account)
	runtime.KeepAlive(secret)
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
	runtime.KeepAlive(service)
	runtime.KeepAlive(account)
	if status != errSecSuccess || item == 0 {
		return fmt.Errorf("Keychain lookup for update failed with OSStatus %d", status)
	}
	defer release(item)
	status = modify(item, 0, uint32(len(secret)), bytesPointer(secret))
	runtime.KeepAlive(secret)
	if status != errSecSuccess {
		return fmt.Errorf("Keychain update failed with OSStatus %d", status)
	}
	return nil
}

func loadDarwinCredential(account string) ([]byte, error) {
	security, err := purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("open Security framework: %w", err)
	}
	defer purego.Dlclose(security)
	coreFoundation, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("open CoreFoundation framework: %w", err)
	}
	defer purego.Dlclose(coreFoundation)
	var find func(uintptr, uint32, uintptr, uint32, uintptr, *uint32, *uintptr, *uintptr) int32
	var freeContent func(uintptr, uintptr) int32
	var open func(uintptr, *uintptr) int32
	var release func(uintptr)
	purego.RegisterLibFunc(&find, security, "SecKeychainFindGenericPassword")
	purego.RegisterLibFunc(&freeContent, security, "SecKeychainItemFreeContent")
	purego.RegisterLibFunc(&open, security, "SecKeychainOpen")
	purego.RegisterLibFunc(&release, coreFoundation, "CFRelease")
	keychain, err := openSelectedKeychain(open)
	if err != nil {
		return nil, err
	}
	if keychain != 0 {
		defer release(keychain)
	}
	var length uint32
	var data uintptr
	var item uintptr
	service := credentialService
	status := find(keychain, uint32(len(service)), stringPointer(service), uint32(len(account)), stringPointer(account), &length, &data, &item)
	runtime.KeepAlive(service)
	runtime.KeepAlive(account)
	if status == errSecItemNotFound {
		return nil, ErrNotFound
	}
	if status != errSecSuccess {
		return nil, fmt.Errorf("Keychain read failed with OSStatus %d", status)
	}
	if item != 0 {
		defer release(item)
	}
	if data == 0 || length == 0 {
		if data != 0 {
			_ = freeContent(0, data)
		}
		return nil, ErrNotFound
	}
	defer freeContent(0, data)
	value := append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(data)), int(length))...)
	return value, nil
}

func deleteDarwinCredential(account string) error {
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
	var find func(uintptr, uint32, uintptr, uint32, uintptr, uintptr, uintptr, *uintptr) int32
	var remove func(uintptr) int32
	var open func(uintptr, *uintptr) int32
	var release func(uintptr)
	purego.RegisterLibFunc(&find, security, "SecKeychainFindGenericPassword")
	purego.RegisterLibFunc(&remove, security, "SecKeychainItemDelete")
	purego.RegisterLibFunc(&open, security, "SecKeychainOpen")
	purego.RegisterLibFunc(&release, coreFoundation, "CFRelease")
	keychain, err := openSelectedKeychain(open)
	if err != nil {
		return err
	}
	if keychain != 0 {
		defer release(keychain)
	}
	var item uintptr
	service := credentialService
	status := find(keychain, uint32(len(service)), stringPointer(service), uint32(len(account)), stringPointer(account), 0, 0, &item)
	runtime.KeepAlive(service)
	runtime.KeepAlive(account)
	if status == errSecItemNotFound {
		return nil
	}
	if status != errSecSuccess || item == 0 {
		return fmt.Errorf("Keychain lookup for deletion failed with OSStatus %d", status)
	}
	defer release(item)
	if status = remove(item); status != errSecSuccess {
		return fmt.Errorf("Keychain deletion failed with OSStatus %d", status)
	}
	return nil
}

func openSelectedKeychain(open func(uintptr, *uintptr) int32) (uintptr, error) {
	path, err := selectedDarwinKeychainPath()
	if err != nil || path == "" {
		return 0, err
	}
	pathBytes := append([]byte(path), 0)
	var keychain uintptr
	status := open(bytesPointer(pathBytes), &keychain)
	runtime.KeepAlive(pathBytes)
	if status != errSecSuccess || keychain == 0 {
		return 0, fmt.Errorf("open test Keychain failed with OSStatus %d", status)
	}
	return keychain, nil
}
