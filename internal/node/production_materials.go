package node

import _ "embed"

var (
	//go:embed materials/blazn-node.service
	productionSystemdUnit []byte
	//go:embed materials/com.blazn.node.plist
	productionLaunchdUnit []byte
)

func ProductionEmbeddedMaterials() map[string][]byte {
	return map[string][]byte{
		"blazn-node-systemd": append([]byte(nil), productionSystemdUnit...),
		"blazn-node-launchd": append([]byte(nil), productionLaunchdUnit...),
	}
}
