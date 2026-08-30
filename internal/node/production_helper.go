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

const RootPrepareStateSubcommand = "node-root-helper-init"

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
	paths, err := HostProductionNodePaths()
	if err != nil || paths.ServiceStateRoot != expected {
		return errors.New("production service state path is invalid")
	}
	if !filepath.IsAbs(binary) || filepath.Clean(binary) != binary {
		return errors.New("node binary path is invalid")
	}
	command := exec.CommandContext(ctx, "/usr/bin/sudo", binary, RootPrepareStateSubcommand)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return errors.New("prepare node service state failed")
	}
	return nil
}

func PrepareProductionServiceState() error {
	if currentUID() != 0 {
		return errors.New("service-state preparation requires UID 0")
	}
	uid, uidErr := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, gidErr := strconv.Atoi(os.Getenv("SUDO_GID"))
	if uidErr != nil || gidErr != nil || uid <= 0 || gid <= 0 {
		return errors.New("service-state preparation requires an authenticated sudo caller")
	}
	paths, err := HostProductionNodePaths()
	if err != nil {
		return err
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
	if err := transitionPrivateStateOwnership(paths.ServiceStateRoot, uid, gid, allowed); err != nil {
		return err
	}
	if paths.ServiceStateRoot == LinuxNodeServiceStateRoot {
		// The signed service parent is daemon-owned only after activation. A
		// recovery or repair first returns it to the authenticated sudo caller
		// so that account remains the sole owner of the 0700/0600 private child.
		parent := filepath.Dir(paths.ServiceStateRoot)
		info, statErr := os.Lstat(parent)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("service-state parent cannot be returned to the authenticated installer")
		}
		owner, _, ownerOK := fileOwner(info)
		if !ownerOK || !allowed[owner] {
			return errors.New("service-state parent cannot be returned to the authenticated installer")
		}
		if err := os.Chown(parent, uid, gid); err != nil {
			return err
		}
		if err := os.Chmod(parent, 0711); err != nil {
			return err
		}
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	value, err := readBoundedRegular(source, 512<<20)
	if err != nil {
		return err
	}
	if existing, readErr := readBoundedRegular(defaultRootBinaryPath, 512<<20); readErr == nil {
		if !bytes.Equal(existing, value) {
			if !rootReceiptOwnsSystemBinary(paths.RootStateRoot, existing) {
				return errors.New("system Blazn binary differs from both the authenticated installer and receipt-owned version")
			}
			if err := writeRootAtomic(defaultRootBinaryPath, value, 0755, 0, 0); err != nil {
				return err
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) && !strings.Contains(readErr.Error(), "material path is unsafe") {
		return readErr
	} else if err := writeRootAtomic(defaultRootBinaryPath, value, 0755, 0, 0); err != nil {
		return err
	}
	return provisionProductionBootstrapProfiles(paths, uid, gid)
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
