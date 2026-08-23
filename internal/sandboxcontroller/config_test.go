package sandboxcontroller

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestConfigFromEnvReadsDatabaseURLOnlyFromSafeFile(t *testing.T) {
	directory := t.TempDir()
	secret := filepath.Join(directory, "database-url")
	if err := os.WriteFile(secret, []byte("postgres://controller:secret@database/blazn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"BLAZN_SANDBOX_CONTROLLER_DATABASE_URL_FILE": secret,
		"BLAZN_SANDBOX_CONTROLLER_WORKER_ID":         "controller-1",
		"BLAZN_SANDBOX_CONTROLLER_LEASE":             "45s",
		"BLAZN_SANDBOX_CONTROLLER_EXPIRY_BATCH":      "12",
	}
	config, err := ConfigFromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.DatabaseURL != "postgres://controller:secret@database/blazn" || config.Controller.Lease != 45*time.Second || config.Controller.ExpiryBatch != 12 {
		t.Fatalf("unexpected config: %#v", config)
	}
}

func TestConfigFromEnvValidatesEffectiveDatabaseLeaseSchedule(t *testing.T) {
	directory := t.TempDir()
	secret := filepath.Join(directory, "database-url")
	if err := os.WriteFile(secret, []byte("postgres://controller@database/blazn"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{
		"BLAZN_SANDBOX_CONTROLLER_DATABASE_URL_FILE": secret,
		"BLAZN_SANDBOX_CONTROLLER_WORKER_ID":         "controller-1",
	}
	for _, test := range []struct {
		name, lease, renew string
	}{
		{name: "fractional lease truncates below renewal", lease: "5.9s", renew: "5.5s"},
		{name: "fractional renewal violates database interval contract", lease: "6s", renew: "5.5s"},
		{name: "renewal consumes lease safety margin", lease: "5.9s", renew: "4s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{}
			for key, value := range base {
				values[key] = value
			}
			values["BLAZN_SANDBOX_CONTROLLER_LEASE"] = test.lease
			values["BLAZN_SANDBOX_CONTROLLER_RENEW_EVERY"] = test.renew
			if _, err := ConfigFromEnv(func(key string) string { return values[key] }); err == nil {
				t.Fatal("fractional database lease schedule was accepted")
			}
		})
	}
	values := map[string]string{}
	for key, value := range base {
		values[key] = value
	}
	values["BLAZN_SANDBOX_CONTROLLER_LEASE"] = "5.9s"
	values["BLAZN_SANDBOX_CONTROLLER_RENEW_EVERY"] = "3s"
	if _, err := ConfigFromEnv(func(key string) string { return values[key] }); err != nil {
		t.Fatalf("safe fractional lease schedule was rejected: %v", err)
	}
}

func TestConfigFromEnvNeverIncludesSecretInErrors(t *testing.T) {
	directory := t.TempDir()
	secret := filepath.Join(directory, "database-url")
	secretValue := "postgres://controller:do-not-log@example.invalid/blazn"
	if err := os.WriteFile(secret, []byte(secretValue), 0o622); err != nil {
		t.Fatal(err)
	}
	_, err := ConfigFromEnv(func(key string) string {
		if key == "BLAZN_SANDBOX_CONTROLLER_DATABASE_URL_FILE" {
			return secret
		}
		return ""
	})
	if err == nil || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("unsafe secret error: %v", err)
	}
}

func TestConfigFromEnvRejectsSymlinkAndDirectURLFallback(t *testing.T) {
	directory := t.TempDir()
	secret := filepath.Join(directory, "database-url")
	link := filepath.Join(directory, "database-url-link")
	if err := os.WriteFile(secret, []byte("postgres://controller@database/blazn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	_, err := ConfigFromEnv(func(key string) string {
		switch key {
		case "BLAZN_SANDBOX_CONTROLLER_DATABASE_URL_FILE":
			return link
		case "DATABASE_URL", "BLAZN_SANDBOX_CONTROLLER_DATABASE_URL":
			return "postgres://must-not-be-used"
		case "BLAZN_SANDBOX_CONTROLLER_WORKER_ID":
			return "controller-1"
		default:
			return ""
		}
	})
	if err == nil {
		t.Fatal("symlinked secret was accepted")
	}
}

func TestSecretFileRejectsGroupWorldBitsWrongOwnerAndHardlinks(t *testing.T) {
	directory := t.TempDir()
	secret := filepath.Join(directory, "database-url")
	if err := os.WriteFile(secret, []byte("postgres://controller@database/blazn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(secret); err == nil {
		t.Fatal("0644 secret file was accepted")
	}
	if err := os.Chmod(secret, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(secret)
	if err != nil {
		t.Fatal(err)
	}
	if secureSecretInfo(info, os.Getuid()+1) {
		t.Fatal("secret file owned by a different expected UID was accepted")
	}
	hardlink := filepath.Join(directory, "database-url-hardlink")
	if err := os.Link(secret, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(secret); err == nil {
		t.Fatal("multiply linked secret file was accepted")
	}
}

func TestSecretFileRejectsSameInodeSymlinkSwapBeforeOpen(t *testing.T) {
	directory := t.TempDir()
	secret := filepath.Join(directory, "database-url")
	target := filepath.Join(directory, "database-url-target")
	if err := os.WriteFile(secret, []byte("postgres://controller@database/blazn"), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := secretFileOps{lstat: os.Lstat, open: func(name string) (*os.File, error) {
		if err := os.Rename(secret, target); err != nil {
			return nil, err
		}
		if err := os.Symlink(target, secret); err != nil {
			return nil, err
		}
		return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	}}
	if _, err := readSecretFileWithOps(secret, ops); err == nil {
		t.Fatal("same-inode symlink substitution was accepted")
	}
}

func TestSecretFileRejectsPostOpenModeAndLinkChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string, string) error
	}{
		{name: "mode", mutate: func(secret, _ string) error { return os.Chmod(secret, 0o640) }},
		{name: "hardlink", mutate: func(secret, other string) error { return os.Link(secret, other) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			secret := filepath.Join(directory, "database-url")
			other := filepath.Join(directory, "database-url-other")
			if err := os.WriteFile(secret, []byte("postgres://controller@database/blazn"), 0o600); err != nil {
				t.Fatal(err)
			}
			ops := secretFileOps{lstat: os.Lstat, open: func(name string) (*os.File, error) {
				return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			}, afterRead: func() error { return test.mutate(secret, other) }}
			if _, err := readSecretFileWithOps(secret, ops); err == nil {
				t.Fatalf("post-open %s change was accepted", test.name)
			}
		})
	}
}
