package node

import "bytes"
import _ "embed"

var (
	//go:embed materials/blazn-node.service
	productionSystemdUnit []byte
	//go:embed materials/com.blazn.node.plist
	productionLaunchdUnit []byte
	//go:embed materials/lima-worker-binding.json
	productionLimaWorkerBindingFile []byte
	//go:embed materials/ubuntu-26.04-amd64-worker-profile.json
	productionLinuxFreshProfile []byte
)

func ProductionEmbeddedMaterials() map[string][]byte {
	return map[string][]byte{
		"blazn-node-systemd":  append([]byte(nil), productionSystemdUnit...),
		"blazn-node-launchd":  append([]byte(nil), productionLaunchdUnit...),
		"lima-worker-binding": append([]byte(nil), bytes.TrimSuffix(productionLimaWorkerBindingFile, []byte("\n"))...),
	}
}

func productionBootstrapProfiles() map[string][]byte {
	return map[string][]byte{
		"ubuntu-26.04-amd64-worker.json": append([]byte(nil), bytes.TrimSpace(productionLinuxFreshProfile)...),
	}
}
