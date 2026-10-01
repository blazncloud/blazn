package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/blazncloud/blazn/internal/client"
)

const (
	RootPrepareStateSubcommand = "node-root-helper-init"
	RootRestoreStateSubcommand = "node-root-helper-restore"
)

func RunProductionRootHelper(ctx context.Context, input io.Reader, output io.Writer) error {
	profileOwner, err := productionTrustedProfileOwner()
	if err != nil {
		return err
	}
	paths, err := HostProductionNodePaths()
	if err != nil {
		return err
	}
	platform := "linux"
	if paths.RootStateRoot == MacOSNodeRootStateRoot {
		platform = "macos"
	}
	engine := NativeRootEngine{
		Platform:            platform,
		Commands:            FixedCommandExecutor{},
		AuthorityPath:       paths.InstallAuthorityPath(),
		ProfileRoot:         paths.ProfileRoot,
		TrustedProfileOwner: profileOwner,
		CurrentBinaryPath:   defaultRootBinaryPath,
	}
	return RunRootHelper(ctx, input, output, engine)
}

func RunProductionObservationHelper(ctx context.Context, output io.Writer) error {
	if currentUID() != 0 {
		return errors.New("root observation helper requires UID 0")
	}
	paths, err := HostProductionNodePaths()
	if err != nil {
		return err
	}
	platform := "linux"
	if paths.RootStateRoot == MacOSNodeRootStateRoot {
		platform = "macos"
	}
	authority, err := loadRootAuthority(paths.InstallAuthorityPath())
	if err != nil {
		return err
	}
	observedIdentity, err := observedIdentityFromAuthority(authority)
	if err != nil {
		return err
	}
	engine := NativeRootEngine{Platform: platform, Commands: FixedCommandExecutor{}, AuthorityPath: paths.InstallAuthorityPath(), ProfileRoot: paths.ProfileRoot, CurrentBinaryPath: defaultRootBinaryPath, RootStateRoot: paths.RootStateRoot, ObservationIdentity: observedIdentity}
	request := RootRequest{SchemaVersion: RootHelperSchema, Operation: RootObserve, Platform: platform, Plan: authority.Plan}
	if err := engine.AuthorizeRootRequest(ctx, request); err != nil {
		return err
	}
	response, err := engine.Execute(ctx, request)
	if err != nil {
		return err
	}
	response.SchemaVersion = RootHelperSchema
	response.OK = true
	return json.NewEncoder(output).Encode(response)
}

func productionTrustedProfileOwner() (int64, error) {
	return trustedProfileOwnerForInvocation(currentUID(), os.Getenv("SUDO_UID"))
}

func trustedProfileOwnerForInvocation(uid int64, sudoUID string) (int64, error) {
	if uid != 0 {
		return uid, nil
	}
	if sudoUID == "" {
		return 0, nil
	}
	callerUID, err := strconv.ParseInt(sudoUID, 10, 64)
	if err != nil || callerUID <= 0 {
		return 0, errors.New("root helper sudo caller is invalid")
	}
	return callerUID, nil
}

func observedIdentityFromAuthority(authority RootInstallAuthority) (RootObservedIdentity, error) {
	publicKey, err := base64.RawURLEncoding.DecodeString(authority.NodePublicKey)
	if err != nil || len(publicKey) != 32 || authority.Identity.Generation < 1 || authority.Identity.SigningKeyID == "" || authority.Plan.EnrollmentID == "" || authority.Plan.NodeID == "" || authority.Plan.WorkspaceID == "" || authority.ControlPlaneOrigin == "" {
		return RootObservedIdentity{}, errors.New("root authority public identity tuple is invalid")
	}
	keyDigest := sha256.Sum256(publicKey)
	fingerprint := "sha256:" + hex.EncodeToString(keyDigest[:])
	if fingerprint != authority.Identity.PublicKeyFingerprint {
		return RootObservedIdentity{}, errors.New("root authority public key fingerprint differs")
	}
	originDigest := sha256.Sum256([]byte(authority.ControlPlaneOrigin))
	return RootObservedIdentity{PublicKey: authority.NodePublicKey, PublicKeyFingerprint: fingerprint, SigningKeyID: authority.Identity.SigningKeyID, Generation: authority.Identity.Generation, EnrollmentID: authority.Plan.EnrollmentID, NodeID: authority.Plan.NodeID, WorkspaceID: authority.Plan.WorkspaceID, ControlPlaneOriginDigest: "sha256:" + hex.EncodeToString(originDigest[:])}, nil
}

func prepareProductionServiceState(ctx context.Context, expected, binary string) error {
	if err := runProductionStateHelper(ctx, expected, binary, RootPrepareStateSubcommand); err != nil {
		return errors.New("prepare node service state failed")
	}
	return nil
}

func restoreProductionServiceState(ctx context.Context, expected, binary string) error {
	if err := runProductionStateHelper(ctx, expected, binary, RootRestoreStateSubcommand); err != nil {
		return errors.New("restore node service state failed")
	}
	return nil
}

func runProductionStateHelper(ctx context.Context, expected, binary, subcommand string) error {
	paths, err := HostProductionNodePaths()
	if err != nil || paths.ServiceStateRoot != expected {
		return errors.New("production service state path is invalid")
	}
	if !filepath.IsAbs(binary) || filepath.Clean(binary) != binary {
		return errors.New("node binary path is invalid")
	}
	command := exec.CommandContext(ctx, "/usr/bin/sudo", binary, subcommand)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func PrepareProductionServiceState() error {
	preparation, err := productionServiceStatePreparation()
	if err != nil {
		return err
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	if preparation.Executable, err = filepath.EvalSymlinks(source); err != nil {
		return err
	}
	return preparation.prepare()
}

// RestoreProductionServiceState returns the daemon's private service state to
// the ownership recorded by the last successful management preparation. The
// CLI calls it when a management command fails after that preparation, so a
// failed uninstall, recover, or repair never leaves the daemon locked out of
// its own state.
func RestoreProductionServiceState() error {
	preparation, err := productionServiceStatePreparation()
	if err != nil {
		return err
	}
	return preparation.restoreRecorded()
}

func productionServiceStatePreparation() (serviceStatePreparation, error) {
	if currentUID() != 0 {
		return serviceStatePreparation{}, errors.New("service-state preparation requires UID 0")
	}
	uid, uidErr := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, gidErr := strconv.Atoi(os.Getenv("SUDO_GID"))
	if uidErr != nil || gidErr != nil || uid <= 0 || gid <= 0 {
		return serviceStatePreparation{}, errors.New("service-state preparation requires an authenticated sudo caller")
	}
	paths, err := HostProductionNodePaths()
	if err != nil {
		return serviceStatePreparation{}, err
	}
	allowed := map[int64]bool{0: true, int64(uid): true}
	serviceName := "blazn-node"
	if paths.ServiceStateRoot == MacOSNodeServiceStateRoot {
		serviceName = "_blazn-node"
	}
	if service, lookupErr := user.Lookup(serviceName); lookupErr == nil {
		if serviceUID, parseErr := strconv.ParseInt(service.Uid, 10, 64); parseErr == nil && serviceUID > 0 {
			allowed[serviceUID] = true
		}
	}
	return serviceStatePreparation{
		Paths:             paths,
		UID:               uid,
		GID:               gid,
		Allowed:           allowed,
		ReturnParent:      paths.ServiceStateRoot == LinuxNodeServiceStateRoot,
		SystemBinaryPath:  defaultRootBinaryPath,
		WriteSystemBinary: func(path string, value []byte) error { return writeRootAtomic(path, value, 0755, 0, 0) },
		ProvisionProfiles: provisionProductionBootstrapProfiles,
	}, nil
}

const serviceStateHandoffName = "service-state-handoff.json"

type serviceStatePreparation struct {
	Paths   ProductionNodePaths
	UID     int
	GID     int
	Allowed map[int64]bool
	// ReturnParent also hands the signed service parent to the caller; it is
	// daemon-owned only after activation.
	ReturnParent      bool
	Executable        string
	SystemBinaryPath  string
	WriteSystemBinary func(string, []byte) error
	ProvisionProfiles func(ProductionNodePaths, int, int) error
}

// prepare hands the private service state to the authenticated sudo caller.
// Every check that can refuse the command runs before the first mutation: a
// management command run by a CLI other than the one the signed plan pins
// must leave the system binary, bootstrap receipt and daemon-owned state
// exactly as it found them. Once mutation starts, any failure returns the
// service state to its recorded ownership.
func (p serviceStatePreparation) prepare() (err error) {
	value, err := readBoundedRegular(p.Executable, 512<<20)
	if err != nil {
		return err
	}
	pinned, err := verifyManagementBinaryPin(p.Paths, value)
	if err != nil {
		return err
	}
	snapshot, err := snapshotServiceStateOwnership(p.Paths.ServiceStateRoot)
	if err != nil {
		return err
	}
	handoff := FileStateStore{Root: p.Paths.RootStateRoot}
	if pinned {
		// Persist the pre-transition ownership before mutating it so the
		// CLI can restore it if a later management step fails.
		if err := handoff.write(serviceStateHandoffName, snapshot); err != nil {
			return fmt.Errorf("record service-state ownership: %w", err)
		}
	}
	defer func() {
		if err == nil {
			return
		}
		if restoreErr := restoreServiceStateOwnership(snapshot, p.restoreAllowed(snapshot)); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore service-state ownership: %w", restoreErr))
			return
		}
		if pinned {
			_ = removeServiceStateHandoff(handoff)
		}
	}()
	if err := transitionPrivateStateOwnership(p.Paths.ServiceStateRoot, p.UID, p.GID, p.Allowed); err != nil {
		return err
	}
	if p.ReturnParent {
		// The signed service parent is daemon-owned only after activation. A
		// recovery or repair first returns it to the authenticated sudo caller
		// so that account remains the sole owner of the 0700/0600 private child.
		parent := filepath.Dir(p.Paths.ServiceStateRoot)
		info, statErr := os.Lstat(parent)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("service-state parent cannot be returned to the authenticated installer")
		}
		owner, _, ownerOK := fileOwner(info)
		if !ownerOK || !p.Allowed[owner] {
			return errors.New("service-state parent cannot be returned to the authenticated installer")
		}
		if err := os.Chown(parent, p.UID, p.GID); err != nil {
			return err
		}
		if err := os.Chmod(parent, 0711); err != nil {
			return err
		}
	}
	if existing, readErr := readBoundedRegular(p.SystemBinaryPath, 512<<20); readErr == nil {
		// With an active pin the executable already equals the pinned
		// binary, so this only repairs a damaged system copy. Without one, a
		// newer installer may replace a binary left by a removed install.
		if !bytes.Equal(existing, value) {
			if !pinned && !rootReceiptOwnsSystemBinary(p.Paths.RootStateRoot, existing) {
				return errors.New("system Blazn binary differs from both the authenticated installer and receipt-owned version")
			}
			if err := p.WriteSystemBinary(p.SystemBinaryPath, value); err != nil {
				return err
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) && !strings.Contains(readErr.Error(), "material path is unsafe") {
		return readErr
	} else if err := p.WriteSystemBinary(p.SystemBinaryPath, value); err != nil {
		return err
	}
	return p.ProvisionProfiles(p.Paths, p.UID, p.GID)
}

func (p serviceStatePreparation) restoreRecorded() error {
	handoff := FileStateStore{Root: p.Paths.RootStateRoot}
	var snapshot serviceStateOwnershipSnapshot
	if err := handoff.read(serviceStateHandoffName, 1<<20, &snapshot); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("load service-state ownership record: %w", err)
	}
	if snapshot.SchemaVersion != 1 || snapshot.Root != p.Paths.ServiceStateRoot {
		return errors.New("service-state ownership record does not match this host")
	}
	if err := restoreServiceStateOwnership(snapshot, p.restoreAllowed(snapshot)); err != nil {
		return err
	}
	return removeServiceStateHandoff(handoff)
}

// restoreAllowed lists the owners a restore may take an entry from: the
// owners the preparation may have produced or found, plus every owner the
// snapshot recorded.
func (p serviceStatePreparation) restoreAllowed(snapshot serviceStateOwnershipSnapshot) map[int64]bool {
	allowed := map[int64]bool{int64(p.UID): true}
	for owner, ok := range p.Allowed {
		allowed[owner] = ok
	}
	allowed[snapshot.Parent.UID] = true
	for _, record := range snapshot.Entries {
		allowed[record.UID] = true
	}
	return allowed
}

func removeServiceStateHandoff(store FileStateStore) error {
	err := os.Remove(filepath.Join(store.Root, serviceStateHandoffName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// verifyManagementBinaryPin refuses an executable that differs from the
// binary pinned by an installed node's signed plan and install receipt, and
// reports whether such a pin exists. A node whose receipt is removed no longer
// pins a binary, so a newer installer may enroll it again.
func verifyManagementBinaryPin(paths ProductionNodePaths, value []byte) (bool, error) {
	receipt, err := (FileStateStore{Root: paths.RootStateRoot}).LoadReceipt()
	hasReceipt := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("load root install receipt: %w", err)
	}
	if hasReceipt && receipt.State == "removed" {
		return false, nil
	}
	authority, err := loadRootAuthority(paths.InstallAuthorityPath())
	hasAuthority := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if !hasAuthority && !hasReceipt {
		return false, nil
	}
	sum := sha256.Sum256(value)
	measured := hex.EncodeToString(sum[:])
	digest := "sha256:" + measured
	if hasAuthority {
		version, pinned := "", ""
		for _, component := range authority.Plan.Components {
			if component.SourceClass == "current_binary" && component.ArtifactType == "binary" {
				if pinned != "" {
					return false, errors.New("signed install plan pins an ambiguous current binary")
				}
				version, pinned = component.Version, component.SHA256
			}
		}
		if pinned == "" {
			return false, errors.New("signed install plan lacks its current binary pin")
		}
		// Plan components carry a bare hex digest; receipts prefix theirs.
		if measured != pinned {
			return false, fmt.Errorf("running Blazn executable %s is not the binary pinned by the signed install plan (version %s, sha256:%s); rerun this command with the pinned CLI", digest, version, pinned)
		}
	}
	if hasReceipt && receipt.Binary.Path == defaultRootBinaryPath && receipt.Binary.Digest != digest {
		return false, fmt.Errorf("running Blazn executable %s is not the binary pinned by the install receipt (%s); rerun this command with the pinned CLI", digest, receipt.Binary.Digest)
	}
	return true, nil
}

type serviceStateOwnership struct {
	UID  int64       `json:"uid"`
	GID  int64       `json:"gid"`
	Mode os.FileMode `json:"mode"`
}

type serviceStateOwnershipSnapshot struct {
	SchemaVersion int                              `json:"schemaVersion"`
	Root          string                           `json:"root"`
	ParentExisted bool                             `json:"parentExisted"`
	RootExisted   bool                             `json:"rootExisted"`
	Parent        serviceStateOwnership            `json:"parent"`
	Entries       map[string]serviceStateOwnership `json:"entries"`
}

const maxServiceStateEntries = 1024

func ownershipOf(info os.FileInfo) (serviceStateOwnership, bool) {
	owner, _, ownerOK := fileOwner(info)
	group, groupOK := fileGroup(info)
	return serviceStateOwnership{UID: owner, GID: group, Mode: info.Mode().Perm()}, ownerOK && groupOK
}

// snapshotServiceStateOwnership records the owner, group and mode of the
// service-state parent and every entry below the state root without following
// links. An entry the transition would refuse is refused here first.
func snapshotServiceStateOwnership(root string) (serviceStateOwnershipSnapshot, error) {
	snapshot := serviceStateOwnershipSnapshot{SchemaVersion: 1, Root: root, Entries: map[string]serviceStateOwnership{}}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return snapshot, errors.New("service-state root path is invalid")
	}
	parent := filepath.Dir(root)
	if parent == root || parent == string(filepath.Separator) {
		return snapshot, errors.New("service-state parent path is invalid")
	}
	parentInfo, err := os.Lstat(parent)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return snapshot, errors.New("service-state parent contains an unsafe ownership boundary")
	}
	var ok bool
	if snapshot.Parent, ok = ownershipOf(parentInfo); !ok {
		return snapshot, errors.New("service-state parent ownership is unavailable")
	}
	snapshot.ParentExisted = true
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return snapshot, errors.New("service-state root is not a directory")
	}
	snapshot.RootExisted = true
	stateRoot, err := os.OpenRoot(root)
	if err != nil {
		return snapshot, err
	}
	defer stateRoot.Close()
	opened, err := stateRoot.Lstat(".")
	if err != nil || !os.SameFile(rootInfo, opened) {
		return snapshot, errors.New("service-state root changed while recording ownership")
	}
	err = fs.WalkDir(stateRoot.FS(), ".", func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := stateRoot.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("service state contains an unsafe ownership boundary")
		}
		record, ok := ownershipOf(info)
		if !ok {
			return errors.New("service-state ownership is unavailable")
		}
		if len(snapshot.Entries) >= maxServiceStateEntries {
			return errors.New("service state exceeds the ownership record limit")
		}
		snapshot.Entries[path] = record
		return nil
	})
	return snapshot, err
}

// restoreServiceStateOwnership returns the service state to a snapshot. An
// entry created after the snapshot takes the recorded root's owner with the
// private mode, so the daemon can still read everything under its root.
func restoreServiceStateOwnership(snapshot serviceStateOwnershipSnapshot, allowed map[int64]bool) error {
	root := snapshot.Root
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.New("service-state root path is invalid")
	}
	parent := filepath.Dir(root)
	if !snapshot.ParentExisted {
		// The transition created both directories. Removing a directory
		// that has gained content fails, which leaves that content in place.
		_ = os.Remove(root)
		_ = os.Remove(parent)
		return nil
	}
	if !snapshot.RootExisted {
		_ = os.Remove(root)
	} else if err := restorePinnedStateTree(snapshot, allowed); err != nil {
		return err
	}
	parentFile, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer parentFile.Close()
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	openedParent, err := parentFile.Stat()
	if err != nil {
		return err
	}
	owner, _, ok := fileOwner(openedParent)
	if !ok || !allowed[owner] || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(parentInfo, openedParent) {
		return errors.New("service-state parent contains an unsafe ownership boundary")
	}
	if err := parentFile.Chown(int(snapshot.Parent.UID), int(snapshot.Parent.GID)); err != nil {
		return err
	}
	return parentFile.Chmod(snapshot.Parent.Mode)
}

func restorePinnedStateTree(snapshot serviceStateOwnershipSnapshot, allowed map[int64]bool) error {
	rootRecord, ok := snapshot.Entries["."]
	if !ok {
		return errors.New("service-state ownership record lacks its root")
	}
	stateRoot, err := os.OpenRoot(snapshot.Root)
	if err != nil {
		return err
	}
	defer stateRoot.Close()
	return fs.WalkDir(stateRoot.FS(), ".", func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := stateRoot.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("service state contains an unsafe ownership boundary")
		}
		file, err := stateRoot.Open(path)
		if err != nil {
			return err
		}
		mutationErr := func() error {
			opened, err := file.Stat()
			if err != nil {
				return err
			}
			owner, links, ok := fileOwner(opened)
			if !ok || !allowed[owner] || !os.SameFile(info, opened) || (!opened.IsDir() && (!opened.Mode().IsRegular() || links != 1)) {
				return errors.New("service state contains an unsafe ownership boundary")
			}
			record, recorded := snapshot.Entries[path]
			if !recorded {
				record = serviceStateOwnership{UID: rootRecord.UID, GID: rootRecord.GID, Mode: 0600}
				if opened.IsDir() {
					record.Mode = 0700
				}
			}
			if err := file.Chown(int(record.UID), int(record.GID)); err != nil {
				return err
			}
			return file.Chmod(record.Mode)
		}()
		closeErr := file.Close()
		if mutationErr != nil {
			return mutationErr
		}
		return closeErr
	})
}

type bootstrapProfileReceipt struct {
	SchemaVersion int    `json:"schemaVersion"`
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	OwnerUID      int    `json:"ownerUid"`
	OwnerGID      int    `json:"ownerGid"`
}

func provisionProductionBootstrapProfiles(paths ProductionNodePaths, uid, gid int) error {
	profiles := productionBootstrapProfiles()
	if paths.ProfileRoot != LinuxNodeProfileRoot || len(profiles) != 1 {
		return nil
	}
	return provisionBootstrapProfiles(paths, uid, gid, 0, 0, profiles)
}

func provisionBootstrapProfiles(paths ProductionNodePaths, uid, gid, systemUID, systemGID int, profiles map[string][]byte) error {
	for _, directory := range []struct {
		path string
		mode os.FileMode
		uid  int
		gid  int
	}{
		{filepath.Dir(filepath.Dir(paths.ProfileRoot)), 0755, systemUID, systemGID},
		{filepath.Dir(paths.ProfileRoot), 0755, systemUID, systemGID},
		{paths.ProfileRoot, 0700, uid, gid},
		{paths.RootStateRoot, 0700, systemUID, systemGID},
	} {
		if err := ensureBootstrapDirectory(directory.path, directory.mode, directory.uid, directory.gid); err != nil {
			return fmt.Errorf("prepare bootstrap profile directory %s: %w", directory.path, err)
		}
	}
	for name, encoded := range profiles {
		path := filepath.Join(paths.ProfileRoot, name)
		_, pathErr := os.Lstat(path)
		if pathErr == nil {
			existing, err := readBoundedRegular(path, 64<<10)
			if err != nil {
				return err
			}
			info, statErr := os.Lstat(path)
			owner, _, ownerOK := fileOwner(info)
			group, groupOK := fileGroup(info)
			if statErr != nil || !ownerOK || !groupOK || owner != int64(uid) || group != int64(gid) || info.Mode().Perm() != 0600 || !bytes.Equal(bytes.TrimSpace(existing), encoded) {
				return errors.New("existing trusted bootstrap profile differs from this signed release")
			}
		} else if errors.Is(pathErr, os.ErrNotExist) {
			if err := writeBootstrapProfileAtomic(path, append(encoded, '\n'), uid, gid); err != nil {
				return err
			}
		} else {
			return pathErr
		}
		digest := sha256.Sum256(encoded)
		receipt, err := json.Marshal(bootstrapProfileReceipt{SchemaVersion: 1, Path: path, SHA256: "sha256:" + hex.EncodeToString(digest[:]), OwnerUID: uid, OwnerGID: gid})
		if err != nil {
			return err
		}
		receiptPath := filepath.Join(paths.RootStateRoot, "bootstrap-profile-receipt.json")
		if systemUID == 0 {
			err = writeRootAtomic(receiptPath, append(receipt, '\n'), 0600, 0, 0)
		} else {
			err = writeBootstrapProfileAtomic(receiptPath, append(receipt, '\n'), systemUID, systemGID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func writeBootstrapProfileAtomic(path string, value []byte, uid, gid int) error {
	directoryPath := filepath.Dir(path)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(directoryPath) == directoryPath {
		return errors.New("bootstrap profile path is unsafe")
	}
	info, err := os.Lstat(directoryPath)
	owner, _, ownerOK := fileOwner(info)
	group, groupOK := fileGroup(info)
	if err != nil || !ownerOK || !groupOK || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || owner != int64(uid) || group != int64(gid) || info.Mode().Perm() != 0700 {
		return errors.New("bootstrap profile directory changed or is unsafe")
	}
	root, err := os.OpenRoot(directoryPath)
	if err != nil {
		return err
	}
	defer root.Close()
	opened, err := root.Lstat(".")
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("bootstrap profile directory changed while opening")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporary := ".blazn-bootstrap-" + hex.EncodeToString(random)
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		_ = root.Remove(temporary)
	}()
	created, err := file.Stat()
	createdOwner, createdLinks, createdOK := fileOwner(created)
	if err != nil || !createdOK || !created.Mode().IsRegular() || createdLinks != 1 || createdOwner != currentUID() {
		return errors.New("bootstrap profile temporary file is unsafe")
	}
	if err := file.Chown(uid, gid); err != nil {
		return err
	}
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(value); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	if err := root.Rename(temporary, filepath.Base(path)); err != nil {
		return err
	}
	directory, err := os.Open(directoryPath)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func ensureBootstrapDirectory(path string, mode os.FileMode, uid, gid int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return errors.New("bootstrap profile directory path is unsafe")
	}
	if err := verifyNoSymlinkTraversal(path); err != nil {
		return err
	}
	created := false
	if err := os.Mkdir(path, mode); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
	} else {
		created = true
	}
	if created {
		if err := os.Chown(path, uid, gid); err != nil {
			return err
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("bootstrap profile directory is unsafe")
	}
	owner, _, ownerOK := fileOwner(info)
	group, groupOK := fileGroup(info)
	if !ownerOK || !groupOK || owner != int64(uid) || group != int64(gid) || info.Mode().Perm() != mode {
		return errors.New("bootstrap profile directory ownership or mode differs")
	}
	return nil
}

func receiptOwnsSystemBinary(receipt client.NodeInstallReceipt, value []byte) bool {
	sum := sha256.Sum256(value)
	return (receipt.State == "active" || receipt.State == "removed") && receipt.Binary.Path == defaultRootBinaryPath && receipt.Binary.Digest == "sha256:"+hex.EncodeToString(sum[:])
}

func rootReceiptOwnsSystemBinary(root string, value []byte) bool {
	if receipt, err := (FileStateStore{Root: root}).LoadReceipt(); err == nil && receiptOwnsSystemBinary(receipt, value) {
		return true
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) > 128 {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "retired-") || !strings.HasSuffix(name, "-receipt.json") || len(name) != len("retired--receipt.json")+36 {
			continue
		}
		encoded, readErr := readPrivateFile(filepath.Join(root, name), 256<<10)
		if readErr != nil {
			continue
		}
		receipt, decodeErr := client.DecodeNodeInstallReceipt(bytes.NewReader(encoded))
		if decodeErr == nil && receiptOwnsSystemBinary(receipt, value) {
			return true
		}
	}
	return false
}

func transitionPrivateStateOwnership(root string, uid, gid int, allowed map[int64]bool) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || uid <= 0 || gid <= 0 || !allowed[int64(uid)] {
		return errors.New("service-state ownership transition is invalid")
	}
	parent := filepath.Dir(root)
	if parent == root || parent == string(filepath.Separator) {
		return errors.New("service-state parent path is invalid")
	}
	if err := os.MkdirAll(parent, 0711); err != nil {
		return err
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	parentFile, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer parentFile.Close()
	openedParentInfo, err := parentFile.Stat()
	if err != nil {
		return err
	}
	parentOwner, _, ok := fileOwner(openedParentInfo)
	if !ok || !allowed[parentOwner] || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(parentInfo, openedParentInfo) {
		return errors.New("service-state parent contains an unsafe ownership boundary")
	}
	if err := parentFile.Chmod(0711); err != nil {
		return err
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer parentRoot.Close()
	pinnedParentInfo, err := parentRoot.Lstat(".")
	if err != nil || !os.SameFile(openedParentInfo, pinnedParentInfo) {
		return errors.New("service-state parent changed during ownership transition")
	}
	rootName := filepath.Base(root)
	if err := parentRoot.Mkdir(rootName, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	rootInfo, err := parentRoot.Lstat(rootName)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("service-state root is not a directory")
	}
	stateRoot, err := parentRoot.OpenRoot(rootName)
	if err != nil {
		return err
	}
	defer stateRoot.Close()
	openedRootInfo, err := stateRoot.Lstat(".")
	if err != nil || !os.SameFile(rootInfo, openedRootInfo) {
		return errors.New("service-state root changed during ownership transition")
	}
	return fs.WalkDir(stateRoot.FS(), ".", func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return transitionPinnedStateEntry(stateRoot, path, uid, gid, allowed, nil)
	})
}

func transitionPinnedStateEntry(root *os.Root, path string, uid, gid int, allowed map[int64]bool, afterLstat func()) error {
	info, err := root.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("service state contains an unsafe ownership boundary")
	}
	if afterLstat != nil {
		afterLstat()
	}
	file, err := root.Open(path)
	if err != nil {
		return err
	}
	mutationErr := func() error {
		opened, err := file.Stat()
		if err != nil {
			return err
		}
		owner, links, ok := fileOwner(opened)
		if !ok || !allowed[owner] || (!opened.IsDir() && (!opened.Mode().IsRegular() || links != 1)) {
			return errors.New("service state contains an unsafe ownership boundary")
		}
		want := os.FileMode(0600)
		if opened.IsDir() {
			want = 0700
		}
		if opened.Mode().Perm() != want {
			return errors.New("service state permissions differ from the private contract")
		}
		if err := file.Chown(uid, gid); err != nil {
			return err
		}
		return file.Chmod(want)
	}()
	closeErr := file.Close()
	if mutationErr != nil {
		return mutationErr
	}
	return closeErr
}
