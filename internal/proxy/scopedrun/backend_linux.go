//go:build linux

package scopedrun

import "github.com/blazncloud/blazn/internal/proxy/credential"

func productionCredentialBackend() credential.Backend {
	return credential.NewSecretServiceBackend()
}
