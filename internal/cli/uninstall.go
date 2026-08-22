package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const installReceiptName = ".blazn-install-receipt"

type UninstallResult struct {
	Command         string `json:"command"`
	Status          string `json:"status"`
	Path            string `json:"path"`
	ConfigPreserved bool   `json:"configPreserved"`
	Residue         string `json:"residue,omitempty"`
}

type uninstallOps struct {
	mkdir  func(string, os.FileMode) error
	rename func(string, string) error
	remove func(string) error
}

var defaultUninstallOps = uninstallOps{mkdir: os.Mkdir, rename: os.Rename, remove: os.Remove}

func RunUninstall() (UninstallResult, error) {
	executable, err := os.Executable()
	if err != nil {
		return UninstallResult{}, fmt.Errorf("resolve executable: %w", err)
	}
	return runUninstallAt(executable)
}

func runUninstallAt(executable string) (UninstallResult, error) {
	return runUninstallAtWithOps(executable, defaultUninstallOps)
}

func runUninstallAtWithOps(executable string, ops uninstallOps) (result UninstallResult, resultErr error) {
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

	directory := filepath.Dir(executable)
	lockDir := filepath.Join(directory, ".blazn-install.lock")
	if err := ops.mkdir(lockDir, 0o700); err != nil {
		if os.IsExist(err) {
			return UninstallResult{}, fmt.Errorf("another or stale Blazn install or uninstall operation owns %s; rerun the signed installer to reconcile stale state", lockDir)
		}
		return UninstallResult{}, fmt.Errorf("create lifecycle lock %s: %w", lockDir, err)
	}
	lockOwned := true
	lockOwner := filepath.Join(lockDir, "owner")
	lockJournal := filepath.Join(lockDir, "journal")
	defer func() {
		if !lockOwned {
			return
		}
		_ = ops.remove(lockOwner)
		_ = ops.remove(lockJournal)
		if err := ops.remove(lockDir); err != nil {
			if result.Status == "removed" {
				result.Status = "removed_with_residue"
				result.Residue = lockDir
				return
			}
			if resultErr == nil {
				resultErr = fmt.Errorf("remove lifecycle lock: %w", err)
			}
		}
	}()
	startIdentity, err := processStartIdentity(os.Getpid())
	if err != nil {
		return UninstallResult{}, fmt.Errorf("determine uninstall process identity: %w", err)
	}
	if err := os.WriteFile(lockOwner, []byte(fmt.Sprintf("pid=%d\nstart=%s\n", os.Getpid(), startIdentity)), 0o600); err != nil {
		return UninstallResult{}, fmt.Errorf("write lifecycle lock owner: %w", err)
	}
	if err := os.WriteFile(lockJournal, []byte("state=uninstall_preparing\nhad_binary=1\nhad_receipt=1\n"), 0o600); err != nil {
		return UninstallResult{}, fmt.Errorf("write uninstall journal: %w", err)
	}

	receipt := filepath.Join(directory, installReceiptName)
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

	stagedReceipt := filepath.Join(directory, ".blazn-uninstall-receipt")
	if _, err := os.Lstat(stagedReceipt); err == nil {
		return UninstallResult{}, fmt.Errorf("stale uninstall receipt exists at %s; rerun the signed installer to reconcile it", stagedReceipt)
	} else if !os.IsNotExist(err) {
		return UninstallResult{}, fmt.Errorf("inspect staged uninstall receipt: %w", err)
	}
	if err := ops.rename(receipt, stagedReceipt); err != nil {
		return UninstallResult{}, fmt.Errorf("stage installation receipt: %w", err)
	}
	if err := ops.remove(executable); err != nil {
		if restoreErr := ops.rename(stagedReceipt, receipt); restoreErr != nil {
			return UninstallResult{
				Command:         "uninstall",
				Status:          "failed_with_residue",
				Path:            executable,
				ConfigPreserved: true,
				Residue:         stagedReceipt,
			}, nil
		}
		return UninstallResult{}, fmt.Errorf("remove executable: %w", err)
	}
	result = UninstallResult{Command: "uninstall", Status: "removed", Path: executable, ConfigPreserved: true}
	if err := ops.remove(stagedReceipt); err != nil {
		result.Status = "removed_with_residue"
		result.Residue = stagedReceipt
		return result, nil
	}

	return result, nil
}

func processStartIdentity(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return "", err
		}
		end := strings.LastIndex(string(data), ") ")
		if end < 0 {
			return "", fmt.Errorf("invalid proc stat format")
		}
		fields := strings.Fields(string(data)[end+2:])
		if len(fields) <= 19 {
			return "", fmt.Errorf("proc stat is missing process start time")
		}
		return fields[19], nil
	}
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", err
	}
	identity := strings.Join(strings.Fields(string(output)), " ")
	if identity == "" {
		return "", fmt.Errorf("process start time is empty")
	}
	return identity, nil
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
		if code := a.writeJSON(result); code != ExitSuccess {
			return code
		}
		if result.Status != "removed" {
			return ExitFailure
		}
		return ExitSuccess
	}
	fmt.Fprintf(a.stdout, "Removed %s\n", result.Path)
	fmt.Fprintln(a.stdout, "Blazn configuration and cache were preserved.")
	if result.Status != "removed" {
		fmt.Fprintf(a.stderr, "blazn: uninstall completed with residue at %s\n", result.Residue)
		return ExitFailure
	}
	return ExitSuccess
}
