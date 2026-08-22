//go:build darwin

package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/creack/pty"
)

func runPasswordPrompt(name string, args []string, secret []byte) error {
	if len(secret) == 0 {
		return errors.New("refusing to submit an empty Keychain credential")
	}
	command := exec.Command(name, args...)
	terminal, err := pty.Start(command)
	if err != nil {
		return fmt.Errorf("start Keychain prompt: %w", err)
	}
	defer terminal.Close()
	_ = terminal.SetReadDeadline(time.Now().Add(5 * time.Second))
	prompt := make([]byte, 0, 1024)
	buffer := make([]byte, 256)
	for len(prompt) < 4096 {
		count, readErr := terminal.Read(buffer)
		if count > 0 {
			prompt = append(prompt, buffer[:count]...)
			if bytes.Contains(bytes.ToLower(prompt), []byte("password")) {
				break
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				_ = command.Wait()
				return errors.New("Keychain prompt exited before requesting the credential")
			}
			_ = command.Process.Kill()
			_ = command.Wait()
			return errors.New("Keychain prompt did not become ready")
		}
	}
	if !bytes.Contains(bytes.ToLower(prompt), []byte("password")) {
		_ = command.Process.Kill()
		_ = command.Wait()
		return errors.New("Keychain prompt exceeded its output limit")
	}
	_ = terminal.SetReadDeadline(time.Time{})
	value := append(append([]byte(nil), secret...), '\n')
	if _, err := terminal.Write(value); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return errors.New("submit Keychain credential through private prompt")
	}
	for index := range value {
		value[index] = 0
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return errors.New("Keychain rejected the credential")
		}
		return nil
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		<-done
		return errors.New("Keychain prompt timed out")
	}
}
