package sandboxcontroller

import (
	"os"
	"path/filepath"
	"strings"
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
