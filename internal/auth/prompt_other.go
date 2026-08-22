//go:build !darwin

package auth

import "errors"

func runPasswordPrompt(string, []string, []byte) error {
	return errors.New("interactive credential prompt is unsupported on this platform")
}
