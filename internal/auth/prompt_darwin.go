//go:build darwin

package auth

import (
	"errors"
	"fmt"
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
	// Input written to the PTY is buffered until security reaches its prompt.
	// Do not read terminal output: some macOS releases do not emit a prompt,
	// and not reading also ensures a terminal echo can never enter our logs.
	time.Sleep(100 * time.Millisecond)
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
