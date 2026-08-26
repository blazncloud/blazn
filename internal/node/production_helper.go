package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	authority, err := loadRootAuthority(paths.InstallAuthorityPath())
	if err != nil {
		return err
	}
	observedIdentity, err := observedIdentityFromAuthority(authority)
	if err != nil {
		return err
	}
	engine := NativeRootEngine{Platform: platform, Commands: FixedCommandExecutor{}, AuthorityPath: paths.InstallAuthorityPath(), ProfileRoot: paths.ProfileRoot, TrustedProfileOwner: profileOwner, CurrentBinaryPath: defaultRootBinaryPath, RootStateRoot: paths.RootStateRoot, ObservationIdentity: observedIdentity}
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
			return writeRootAtomic(defaultRootBinaryPath, value, 0755, 0, 0)
		}
		return nil
	} else if !errors.Is(readErr, os.ErrNotExist) && !strings.Contains(readErr.Error(), "material path is unsafe") {
		return readErr
	}
	return writeRootAtomic(defaultRootBinaryPath, value, 0755, 0, 0)
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
