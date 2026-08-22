package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

const (
	defaultRootAuthorityPath = "/var/lib/blazn/root/install-authority.json"
	defaultRootProfileRoot   = "/etc/blazn/node/profiles"
	defaultRootBinaryPath    = "/usr/local/bin/blazn"
)

func (e NativeRootEngine) authorizeBootstrap(ctx context.Context, request RootRequest) error {
	bootstrap := request.Bootstrap
	if bootstrap == nil {
		return errors.New("root bootstrap request is missing")
	}
	authorization := BootstrapAuthorization{EnrollmentID: bootstrap.EnrollmentID, Token: bootstrap.Token, MachineFingerprint: bootstrap.MachineFingerprint, NodePublicKey: bootstrap.NodePublicKey, Platform: bootstrap.Platform, Architecture: bootstrap.Architecture, KubernetesBinding: bootstrap.KubernetesBinding, PlanSigningKey: bootstrap.PlanSigningKey, Expected: bootstrap.Expected, ProfileID: bootstrap.ProfileID, ProfilePath: bootstrap.ProfilePath}
	if err := authorization.Validate(); err != nil {
		return err
	}
	if request.Plan.Digest != authorization.Expected.Plan.Digest || !sameJSON(request.Plan, authorization.Expected.Plan) {
		return errors.New("root bootstrap plan differs from expected exchange")
	}
	profileRoot, binaryPath, authorityPath := e.authorityPaths()
	if filepath.Dir(authorization.ProfilePath) != profileRoot {
		return errors.New("root bootstrap profile is outside the fixed trust root")
	}
	profileBytes, err := readTrustedProfile(authorization.ProfilePath)
	if err != nil {
		return err
	}
	profileDigest := sha256.Sum256(profileBytes)
	profileSHA256 := "sha256:" + hex.EncodeToString(profileDigest[:])
	profile, err := LoadTrustedProfile(authorization.ProfilePath, binaryPath, currentBinaryVersion(request.Plan))
	if err != nil {
		return err
	}
	if profile.ID != authorization.ProfileID {
		return errors.New("root bootstrap profile ID differs from expected plan")
	}
	replayRequest := client.ExchangeNodeEnrollmentRequest{Token: bootstrap.Token, MachineFingerprint: bootstrap.MachineFingerprint, NodePublicKey: bootstrap.NodePublicKey, Platform: bootstrap.Platform, Architecture: bootstrap.Architecture, KubernetesBinding: bootstrap.KubernetesBinding}
	httpClient := e.AuthorityHTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Transport: &http.Transport{Proxy: nil, DisableCompression: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	clientCopy.Timeout = 30 * time.Second
	api, err := client.New(profile.ControlPlaneOrigin, &clientCopy)
	if err != nil {
		return err
	}
	replayContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	replayed, err := api.ExchangeNodeEnrollment(replayContext, bootstrap.EnrollmentID, replayRequest)
	bootstrap.Token = ""
	if err != nil {
		return errors.New("root enrollment exchange replay failed")
	}
	if !sameJSON(replayed, authorization.Expected) {
		return errors.New("root enrollment replay differs from expected exchange")
	}
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	authority := RootInstallAuthority{SchemaVersion: RootInstallAuthoritySchema, Plan: replayed.Plan, Identity: replayed.Identity, PlanSigningKey: bootstrap.PlanSigningKey, NodePublicKey: bootstrap.NodePublicKey, KubernetesBinding: bootstrap.KubernetesBinding, ProfileID: profile.ID, ProfileSHA256: profileSHA256, ControlPlaneOrigin: profile.ControlPlaneOrigin, AuthorizedAt: now.UTC().Format(time.RFC3339Nano)}
	authority.Digest, err = RootInstallAuthorityDigest(authority)
	if err != nil {
		return err
	}
	if err := VerifyRootInstallAuthority(authority, RootInstallAuthorityTrust{Now: now, Profile: profile, ProfileSHA256: profileSHA256}); err != nil {
		return err
	}
	encoded, err := json.Marshal(authority)
	if err != nil {
		return err
	}
	if err := writePrivateCreate(authorityPath, encoded); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, readErr := readPrivateFile(authorityPath, 8<<20)
		if readErr != nil {
			return readErr
		}
		prior, decodeErr := DecodeRootInstallAuthority(existing)
		if decodeErr != nil || !sameAuthorityBinding(prior, authority) {
			return errors.New("root install authority is already bound to different trust")
		}
	}
	return nil
}

func (e NativeRootEngine) AuthorizeRootRequest(_ context.Context, request RootRequest) error {
	if request.Bootstrap != nil {
		return errors.New("bootstrap secret is not accepted for privileged mutations")
	}
	profileRoot, binaryPath, authorityPath := e.authorityPaths()
	encoded, err := readPrivateFile(authorityPath, 8<<20)
	if err != nil {
		return fmt.Errorf("load root install authority: %w", err)
	}
	authority, err := DecodeRootInstallAuthority(encoded)
	if err != nil {
		return err
	}
	if request.Plan.Digest != authority.Plan.Digest || !sameJSON(request.Plan, authority.Plan) {
		return errors.New("privileged request plan differs from root install authority")
	}
	profile, profileSHA256, err := loadAuthorityProfile(profileRoot, binaryPath, authority)
	if err != nil {
		return err
	}
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	return VerifyRootInstallAuthority(authority, RootInstallAuthorityTrust{Now: now, Profile: profile, ProfileSHA256: profileSHA256})
}

func (e NativeRootEngine) authorityPaths() (string, string, string) {
	profileRoot, binaryPath, authorityPath := e.ProfileRoot, e.CurrentBinaryPath, e.AuthorityPath
	if profileRoot == "" {
		profileRoot = defaultRootProfileRoot
	}
	if binaryPath == "" {
		binaryPath = defaultRootBinaryPath
	}
	if authorityPath == "" {
		authorityPath = defaultRootAuthorityPath
	}
	return profileRoot, binaryPath, authorityPath
}

func loadAuthorityProfile(root, binaryPath string, authority RootInstallAuthority) (client.NodeTrustedInstallProfile, string, error) {
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return client.NodeTrustedInstallProfile{}, "", errors.New("root trusted profile directory is unsafe")
	}
	if err := ensurePrivateDirectory(root, currentUID()); err != nil {
		return client.NodeTrustedInstallProfile{}, "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) > 32 {
		return client.NodeTrustedInstallProfile{}, "", errors.New("root trusted profile directory is unavailable or exceeds limit")
	}
	var selected client.NodeTrustedInstallProfile
	matches := 0
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(root, entry.Name())
		value, readErr := readTrustedProfile(path)
		if readErr != nil {
			return client.NodeTrustedInstallProfile{}, "", readErr
		}
		sum := sha256.Sum256(value)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		if digest != authority.ProfileSHA256 {
			continue
		}
		profile, loadErr := LoadTrustedProfile(path, binaryPath, currentBinaryVersion(authority.Plan))
		if loadErr != nil {
			return client.NodeTrustedInstallProfile{}, "", loadErr
		}
		if profile.ID == authority.ProfileID && profile.ControlPlaneOrigin == authority.ControlPlaneOrigin {
			selected = profile
			matches++
		}
	}
	if matches != 1 {
		return client.NodeTrustedInstallProfile{}, "", errors.New("root install authority profile is missing or ambiguous")
	}
	return selected, authority.ProfileSHA256, nil
}

func currentBinaryVersion(plan client.NodeInstallPlan) string {
	version := ""
	for _, component := range plan.Components {
		if component.SourceClass == "current_binary" && component.ArtifactType == "binary" {
			if version != "" {
				return ""
			}
			version = component.Version
		}
	}
	return version
}

func sameJSON(left, right any) bool {
	a, aErr := json.Marshal(left)
	b, bErr := json.Marshal(right)
	return aErr == nil && bErr == nil && bytes.Equal(a, b)
}

func sameAuthorityBinding(left, right RootInstallAuthority) bool {
	left.AuthorizedAt, left.Digest = "", ""
	right.AuthorizedAt, right.Digest = "", ""
	return sameJSON(left, right)
}
