//go:build !linux && !darwin

package harnessworker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
)

func openNoFollow(string) (*os.File, error) {
	return nil, errors.New("protected listener token files are unsupported on this platform")
}

func protectedInfo(info os.FileInfo, maxBytes int64) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o400 && info.Size() > 0 && info.Size() <= maxBytes
}

func digestBytes(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}
