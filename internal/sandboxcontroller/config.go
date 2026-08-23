package sandboxcontroller

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const maxDatabaseURLBytes = 16 * 1024

type RuntimeConfig struct {
	Controller  Config
	DatabaseURL string
}

func ConfigFromEnv(getenv func(string) string) (RuntimeConfig, error) {
	if getenv == nil {
		return RuntimeConfig{}, errors.New("environment reader is required")
	}
	databaseURL, err := readSecretFile(getenv("BLAZN_SANDBOX_CONTROLLER_DATABASE_URL_FILE"))
	if err != nil {
		return RuntimeConfig{}, err
	}
	config := Config{
		WorkerID:         getenv("BLAZN_SANDBOX_CONTROLLER_WORKER_ID"),
		Lease:            30 * time.Second,
		RenewEvery:       10 * time.Second,
		PollEvery:        time.Second,
		OperationTimeout: 15 * time.Minute,
		IdleDelay:        time.Second,
		RetryDelay:       10 * time.Second,
		ExpiryEvery:      30 * time.Second,
		ExpiryBatch:      25,
	}
	durations := []struct {
		name   string
		target *time.Duration
	}{
		{"BLAZN_SANDBOX_CONTROLLER_LEASE", &config.Lease},
		{"BLAZN_SANDBOX_CONTROLLER_RENEW_EVERY", &config.RenewEvery},
		{"BLAZN_SANDBOX_CONTROLLER_POLL_EVERY", &config.PollEvery},
		{"BLAZN_SANDBOX_CONTROLLER_OPERATION_TIMEOUT", &config.OperationTimeout},
		{"BLAZN_SANDBOX_CONTROLLER_IDLE_DELAY", &config.IdleDelay},
		{"BLAZN_SANDBOX_CONTROLLER_RETRY_DELAY", &config.RetryDelay},
		{"BLAZN_SANDBOX_CONTROLLER_EXPIRY_EVERY", &config.ExpiryEvery},
	}
	for _, entry := range durations {
		if value := getenv(entry.name); value != "" {
			parsed, parseErr := time.ParseDuration(value)
			if parseErr != nil {
				return RuntimeConfig{}, fmt.Errorf("%s is invalid", entry.name)
			}
			*entry.target = parsed
		}
	}
	if value := getenv("BLAZN_SANDBOX_CONTROLLER_EXPIRY_BATCH"); value != "" {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil {
			return RuntimeConfig{}, errors.New("BLAZN_SANDBOX_CONTROLLER_EXPIRY_BATCH is invalid")
		}
		config.ExpiryBatch = parsed
	}
	if err := validateConfig(config); err != nil {
		return RuntimeConfig{}, err
	}
	return RuntimeConfig{Controller: config, DatabaseURL: databaseURL}, nil
}

func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("BLAZN_SANDBOX_CONTROLLER_DATABASE_URL_FILE is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("sandbox controller database URL file cannot be inspected")
	}
	if info.Mode()&os.ModeSymlink != 0 || !secureSecretInfo(info, os.Getuid()) {
		return "", errors.New("sandbox controller database URL file is unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("sandbox controller database URL file cannot be read")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !secureSecretInfo(opened, os.Getuid()) {
		return "", errors.New("sandbox controller database URL file changed during inspection")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxDatabaseURLBytes+1))
	if err != nil || len(contents) > maxDatabaseURLBytes {
		return "", errors.New("sandbox controller database URL file cannot be read")
	}
	value := strings.TrimSpace(string(contents))
	if value == "" || strings.ContainsRune(value, '\x00') {
		return "", errors.New("sandbox controller database URL file is invalid")
	}
	return value, nil
}

func secureSecretInfo(info os.FileInfo, expectedUID int) bool {
	if info == nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxDatabaseURLBytes || info.Mode().Perm()&0o077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(expectedUID) && uint64(stat.Nlink) == 1
}
