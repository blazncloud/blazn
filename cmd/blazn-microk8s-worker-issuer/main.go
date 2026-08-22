//go:build linux

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/KingJammin/blazn/internal/microk8sissuer"
	"os"
	"strings"
	"syscall"
	"time"
)

const (
	configPath = "/etc/blazn/microk8s-worker-issuer/config.json"
	keyPath    = "/etc/blazn/microk8s-worker-issuer/issuer-hmac-v1"
	socketPath = "/run/blazn/microk8s-worker-issuer.sock"
	stateRoot  = "/var/lib/blazn/microk8s-worker-issuer"
	tokenFile  = "/var/snap/microk8s/current/credentials/cluster-tokens.txt"
)

type config struct {
	SchemaVersion string `json:"schemaVersion"`
	BrokerUID     uint32 `json:"brokerUid"`
	BrokerGID     uint32 `json:"brokerGid"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "MicroK8s worker issuer failed")
		os.Exit(1)
	}
}
func run() error {
	var cfg config
	if err := readSecureJSON(configPath, &cfg); err != nil {
		return err
	}
	if cfg.SchemaVersion != "blazn.dev/microk8s-worker-issuer-config/v1" || cfg.BrokerUID == 0 || cfg.BrokerGID == 0 {
		return fmt.Errorf("invalid config")
	}
	encoded, err := readSecure(keyPath)
	if err != nil {
		return err
	}
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(key) != 32 {
		return fmt.Errorf("invalid key")
	}
	backend := &microk8sissuer.MicroK8sBackend{AddNodePath: "/snap/bin/microk8s.add-node", TokenFile: tokenFile, ExpectedUID: 0, Runner: microk8sissuer.ExecRunner{}}
	service, err := microk8sissuer.NewService(stateRoot, key, backend)
	if err != nil {
		return err
	}
	return (&microk8sissuer.Server{Service: service, AllowedUID: cfg.BrokerUID, AllowedGID: cfg.BrokerGID, Timeout: 10 * time.Second}).Serve(socketPath)
}
func readSecureJSON(path string, out any) error {
	data, err := readSecure(path)
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || len(raw) != 3 || raw["schemaVersion"] == nil || raw["brokerUid"] == nil || raw["brokerGid"] == nil {
		return fmt.Errorf("invalid config")
	}
	return json.Unmarshal(data, out)
}
func readSecure(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || st.Uid != 0 || st.Nlink != 1 || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("unsafe issuer file")
	}
	return os.ReadFile(path)
}
