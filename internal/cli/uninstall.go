package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const installReceiptName = ".blazn-install-receipt"

type UninstallResult struct {
	Command         string `json:"command"`
	Status          string `json:"status"`
	Path            string `json:"path"`
	ConfigPreserved bool   `json:"configPreserved"`
}

func RunUninstall() (UninstallResult, error) {
	executable, err := os.Executable()
	if err != nil {
		return UninstallResult{}, fmt.Errorf("resolve executable: %w", err)
	}
	return runUninstallAt(executable)
}

func runUninstallAt(executable string) (UninstallResult, error) {
	executable, err := filepath.Abs(executable)
	if err != nil {
		return UninstallResult{}, fmt.Errorf("resolve executable path: %w", err)
	}
	info, err := os.Lstat(executable)
	if err != nil {
		return UninstallResult{}, fmt.Errorf("inspect executable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return UninstallResult{}, fmt.Errorf("refusing to uninstall a non-regular executable")
	}

	receipt := filepath.Join(filepath.Dir(executable), installReceiptName)
	receiptInfo, err := os.Lstat(receipt)
	if err != nil {
		if os.IsNotExist(err) {
			return UninstallResult{}, fmt.Errorf("installation receipt not found; use the package manager that installed %s", executable)
		}
		return UninstallResult{}, fmt.Errorf("inspect installation receipt: %w", err)
	}
	if receiptInfo.Mode()&os.ModeSymlink != 0 || !receiptInfo.Mode().IsRegular() {
		return UninstallResult{}, fmt.Errorf("refusing an unsafe installation receipt")
	}

	receiptFile, err := os.Open(receipt)
	if err != nil {
		return UninstallResult{}, fmt.Errorf("open installation receipt: %w", err)
	}
	values, parseErr := parseReceipt(receiptFile)
	closeErr := receiptFile.Close()
	if parseErr != nil {
		return UninstallResult{}, parseErr
	}
	if closeErr != nil {
		return UninstallResult{}, fmt.Errorf("close installation receipt: %w", closeErr)
	}

	want := values["binary_sha256"]
	if len(want) != sha256.Size*2 {
		return UninstallResult{}, fmt.Errorf("installation receipt has an invalid binary checksum")
	}
	got, err := fileSHA256(executable)
	if err != nil {
		return UninstallResult{}, err
	}
	if !strings.EqualFold(got, want) {
		return UninstallResult{}, fmt.Errorf("installed binary differs from its receipt; refusing to remove it")
	}

	stagedReceipt := fmt.Sprintf("%s.removing-%d", receipt, os.Getpid())
	if err := os.Rename(receipt, stagedReceipt); err != nil {
		return UninstallResult{}, fmt.Errorf("stage installation receipt: %w", err)
	}
	if err := os.Remove(executable); err != nil {
		_ = os.Rename(stagedReceipt, receipt)
		return UninstallResult{}, fmt.Errorf("remove executable: %w", err)
	}
	if err := os.Remove(stagedReceipt); err != nil {
		return UninstallResult{}, fmt.Errorf("remove staged installation receipt: %w", err)
	}

	return UninstallResult{Command: "uninstall", Status: "removed", Path: executable, ConfigPreserved: true}, nil
}

func parseReceipt(reader io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("installation receipt contains an invalid record")
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("installation receipt contains duplicate field %q", key)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read installation receipt: %w", err)
	}
	return values, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open executable for verification: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", fmt.Errorf("hash executable: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close executable after verification: %w", closeErr)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (a *App) writeUninstall(format OutputFormat) int {
	result, err := a.uninstall()
	if err != nil {
		return a.writeError(format, ExitFailure, "uninstall_failed", err.Error())
	}
	if format == OutputJSON {
		return a.writeJSON(result)
	}
	fmt.Fprintf(a.stdout, "Removed %s\n", result.Path)
	fmt.Fprintln(a.stdout, "Blazn configuration and cache were preserved.")
	return ExitSuccess
}
