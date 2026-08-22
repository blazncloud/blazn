package node

import (
	"errors"
	"path/filepath"
	"runtime"

	"github.com/blazncloud/blazn/internal/client"
)

const (
	LinuxNodeServiceStateRoot    = "/var/lib/blazn/node"
	LinuxNodeRootStateRoot       = "/var/lib/blazn-node-root"
	LinuxNodeProfileRoot         = "/etc/blazn/node/profiles"
	LinuxMicroK8sIssuerStateRoot = "/var/lib/blazn-node-root/microk8s-worker-issuer"

	MacOSNodeServiceStateRoot = "/Library/Application Support/Blazn/Node"
	MacOSNodeRootStateRoot    = "/Library/Application Support/BlaznNodeRoot"
	MacOSNodeProfileRoot      = "/Library/Application Support/BlaznNodeRoot/profiles"
)

type ProductionNodePaths struct {
	ServiceStateRoot string
	RootStateRoot    string
	ProfileRoot      string
}

func (p ProductionNodePaths) InstallAuthorityPath() string {
	return filepath.Join(p.RootStateRoot, "install-authority.json")
}
func (p ProductionNodePaths) InstallWALPath() string {
	return filepath.Join(p.RootStateRoot, "install-wal.json")
}
func (p ProductionNodePaths) InstallReceiptPath() string {
	return filepath.Join(p.RootStateRoot, "install-receipt.json")
}
func (p ProductionNodePaths) InstallBackupRoot() string {
	return filepath.Join(p.RootStateRoot, "install-backups")
}

func NodeProductionPaths(platform client.NodePlatform) (ProductionNodePaths, error) {
	switch platform {
	case client.NodePlatformLinux:
		return ProductionNodePaths{ServiceStateRoot: LinuxNodeServiceStateRoot, RootStateRoot: LinuxNodeRootStateRoot, ProfileRoot: LinuxNodeProfileRoot}, nil
	case client.NodePlatformMacOS:
		return ProductionNodePaths{ServiceStateRoot: MacOSNodeServiceStateRoot, RootStateRoot: MacOSNodeRootStateRoot, ProfileRoot: MacOSNodeProfileRoot}, nil
	default:
		return ProductionNodePaths{}, errors.New("production Node platform is unsupported")
	}
}

func HostProductionNodePaths() (ProductionNodePaths, error) {
	if runtime.GOOS == "linux" {
		return NodeProductionPaths(client.NodePlatformLinux)
	}
	if runtime.GOOS == "darwin" {
		return NodeProductionPaths(client.NodePlatformMacOS)
	}
	return ProductionNodePaths{}, errors.New("production Node host platform is unsupported")
}
