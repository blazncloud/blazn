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
)

func ProductionEmbeddedMaterials() map[string][]byte {
	return map[string][]byte{
		"blazn-node-systemd":  append([]byte(nil), productionSystemdUnit...),
		"blazn-node-launchd":  append([]byte(nil), productionLaunchdUnit...),
		"lima-worker-binding": append([]byte(nil), bytes.TrimSuffix(productionLimaWorkerBindingFile, []byte("\n"))...),
	}
}
