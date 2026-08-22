package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

type RootEngine interface {
	Execute(context.Context, RootRequest) (RootResponse, error)
}
type RootRequestAuthorizer interface {
	AuthorizeRootRequest(context.Context, RootRequest) error
}

func RunRootHelper(ctx context.Context, input io.Reader, output io.Writer, engine RootEngine) error {
	if currentUID() != 0 {
		return errors.New("root helper requires UID 0")
	}
	data, err := io.ReadAll(io.LimitReader(input, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return errors.New("root helper request exceeds limit")
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return errors.New("root helper request is invalid")
	}
	allowed := map[string]bool{"schemaVersion": true, "operation": true, "platform": true, "plan": true, "ordinal": true, "backupRoot": true, "prior": true, "material": true, "join": true, "bootstrap": true}
	for key := range raw {
		if !allowed[key] {
			return fmt.Errorf("root helper field %q is unsupported", key)
		}
	}
	var request RootRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	if request.SchemaVersion != RootHelperSchema || (request.Platform != "linux" && request.Platform != "macos") {
		return errors.New("root helper binding is invalid")
	}
	if err := validateRootRequestShape(request); err != nil {
		return err
	}
	if err := client.ValidateNodeInstallPlan(request.Plan); err != nil {
		return err
	}
	if (request.Platform == "linux" && request.Plan.Target.Platform != client.NodePlatformLinux) || (request.Platform == "macos" && request.Plan.Target.Platform != client.NodePlatformMacOS) {
		return errors.New("root helper plan platform mismatch")
	}
	if request.Operation == RootAuthorize {
		if request.Bootstrap == nil {
			return errors.New("root bootstrap authorization is missing")
		}
	} else {
		authorizer, ok := engine.(RootRequestAuthorizer)
		if !ok {
			return errors.New("root helper authorizer is unavailable")
		}
		if err := authorizer.AuthorizeRootRequest(ctx, request); err != nil {
			return err
		}
		if err := validateRootRequestMaterial(request); err != nil {
			return err
		}
	}
	if engine == nil {
		return errors.New("root helper engine is unavailable")
	}
	response, err := engine.Execute(ctx, request)
	if err != nil {
		return err
	}
	response.SchemaVersion = RootHelperSchema
	response.OK = true
	return json.NewEncoder(output).Encode(response)
}

func validateRootRequestShape(request RootRequest) error {
	noMutationFields := func() bool {
		return request.Ordinal == 0 && request.BackupRoot == "" && request.Prior == nil && request.Material == nil && request.Join == nil
	}
	switch request.Operation {
	case RootAuthorize:
		if request.Bootstrap == nil || !noMutationFields() {
			return errors.New("root authorize request fields are invalid")
		}
	case RootProbe, RootServiceState:
		if request.Bootstrap != nil || !noMutationFields() {
			return errors.New("root probe request fields are invalid")
		}
	case RootCapture:
		if request.Bootstrap != nil || request.Ordinal < 1 || request.BackupRoot == "" || request.Prior != nil || request.Material != nil || request.Join != nil {
			return errors.New("root capture request fields are invalid")
		}
	case RootApply:
		if request.Bootstrap != nil || request.Ordinal < 1 || request.BackupRoot != "" || request.Prior != nil {
			return errors.New("root apply request fields are invalid")
		}
	case RootRollback:
		if request.Bootstrap != nil || request.Ordinal < 1 || request.BackupRoot == "" || request.Prior == nil || request.Material != nil {
			return errors.New("root rollback request fields are invalid")
		}
	case RootJoin:
		if request.Bootstrap != nil || request.Ordinal != 0 || request.BackupRoot != "" || request.Prior != nil || request.Material != nil || request.Join == nil {
			return errors.New("root join request fields are invalid")
		}
	case RootVerify:
		if request.Bootstrap != nil || request.Ordinal != 0 || request.BackupRoot != "" || request.Prior != nil || request.Material != nil || request.Join == nil {
			return errors.New("root verify request fields are invalid")
		}
	default:
		return errors.New("root helper operation is unsupported")
	}
	return nil
}

func validateRootRequestMaterial(request RootRequest) error {
	if request.Operation != RootApply {
		return nil
	}
	mutation, err := mutationByOrdinal(request.Plan, request.Ordinal)
	if err != nil {
		return err
	}
	name, _ := mutation.Desired["sourceComponent"].(string)
	if name == "" {
		name, _ = mutation.Desired["componentName"].(string)
	}
	if name == "" {
		if request.Material != nil {
			return errors.New("unrequested root material is present")
		}
		return nil
	}
	if request.Material == nil || request.Material.ComponentName != name {
		return errors.New("root material component binding is invalid")
	}
	for _, component := range request.Plan.Components {
		if component.Name == name {
			if request.Material.SHA256 != component.SHA256 {
				return errors.New("root material digest binding is invalid")
			}
			return nil
		}
	}
	return errors.New("root material component is absent from signed plan")
}

type CommandExecutor interface {
	Run(context.Context, string, ...string) ([]byte, error)
	RunInput(context.Context, string, []byte, ...string) ([]byte, error)
}
type FixedCommandExecutor struct{}
type FixedCommandError struct{ ExitCode int }

func (e *FixedCommandError) Error() string { return "fixed privileged command failed" }

func (FixedCommandExecutor) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	return (FixedCommandExecutor{}).RunInput(ctx, path, nil, args...)
}
func (FixedCommandExecutor) RunInput(ctx context.Context, path string, input []byte, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, path, args...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/bin", "LANG=C", "LC_ALL=C", "SNAP=/snap/microk8s/current", "SNAP_DATA=/var/snap/microk8s/current", "SNAP_COMMON=/var/snap/microk8s/common"}
	command.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	command.Stdout = &limitedOutput{writer: &stdout, remaining: 1 << 20}
	command.Stderr = &limitedOutput{writer: &bytes.Buffer{}, remaining: 4096}
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil, &FixedCommandError{ExitCode: exitError.ExitCode()}
		}
		return nil, errors.New("fixed privileged command failed")
	}
	return stdout.Bytes(), nil
}

type NativeRootEngine struct {
	Platform             string
	Commands             CommandExecutor
	Now                  func() time.Time
	LimaBindingPath      string
	allowTestJoinRuntime bool
	AuthorityPath        string
	ProfileRoot          string
	CurrentBinaryPath    string
	AuthorityHTTPClient  *http.Client
}

func (e NativeRootEngine) Execute(ctx context.Context, request RootRequest) (RootResponse, error) {
	if e.Commands == nil {
		e.Commands = FixedCommandExecutor{}
	}
	switch request.Operation {
	case RootAuthorize:
		return RootResponse{}, e.authorizeBootstrap(ctx, request)
	case RootProbe:
		if e.Platform != request.Platform {
			return RootResponse{}, errors.New("root helper OS mismatch")
		}
		binding, err := e.rootKubernetesBinding()
		if err == nil && binding != nil {
			observed, observeErr := e.observeNode(ctx, request.Plan, binding.NodeName)
			if observeErr != nil || observed.UID != binding.NodeUID {
				return RootResponse{}, errors.New("root probe Kubernetes binding differs from authority")
			}
			binding, err = e.updateRootKubernetesBinding(request.Plan, observed)
		}
		return RootResponse{KubernetesBinding: binding}, err
	case RootServiceState:
		return e.serviceState(ctx, request.Plan.NodeService)
	case RootCapture:
		mutation, err := mutationByOrdinal(request.Plan, request.Ordinal)
		if err != nil {
			return RootResponse{}, err
		}
		prior, err := e.capture(ctx, request.Plan, mutation, request.BackupRoot)
		return RootResponse{Prior: &prior}, err
	case RootApply:
		mutation, err := mutationByOrdinal(request.Plan, request.Ordinal)
		if err != nil {
			return RootResponse{}, err
		}
		if err := e.apply(ctx, request.Plan, mutation, request.Material, request.Join); err != nil {
			return RootResponse{}, err
		}
		if mutation.Kind == "label" || mutation.Kind == "taint" {
			observed, err := e.observeNode(ctx, request.Plan, request.Join.ExpectedNodeName)
			if err != nil {
				return RootResponse{}, err
			}
			binding, err := e.updateRootKubernetesBinding(request.Plan, observed)
			return RootResponse{KubernetesBinding: binding}, err
		}
		return RootResponse{}, nil
	case RootRollback:
		mutation, err := mutationByOrdinal(request.Plan, request.Ordinal)
		if err != nil || request.Prior == nil {
			return RootResponse{}, errors.New("rollback request is incomplete")
		}
		if err := e.rollback(ctx, request.Plan, mutation, *request.Prior, request.BackupRoot, request.Join); err != nil {
			return RootResponse{}, err
		}
		if mutation.Kind == "label" || mutation.Kind == "taint" {
			observed, err := e.observeNode(ctx, request.Plan, request.Join.ExpectedNodeName)
			if err != nil {
				return RootResponse{}, err
			}
			binding, err := e.updateRootKubernetesBinding(request.Plan, observed)
			return RootResponse{KubernetesBinding: binding}, err
		}
		return RootResponse{}, nil
	case RootJoin:
		if existing, err := e.rootKubernetesBinding(); err != nil {
			return RootResponse{}, err
		} else if existing != nil {
			observed, observeErr := e.observeNode(ctx, request.Plan, existing.NodeName)
			if observeErr != nil || observed.UID != existing.NodeUID {
				return RootResponse{}, errors.New("existing joined Node differs from root authority")
			}
			return RootResponse{NodeUID: observed.UID, NodeName: observed.Name, ResourceVersion: observed.ResourceVersion, KubernetesBinding: existing}, nil
		}
		joined, err := e.join(ctx, request.Plan, request.Join)
		if err != nil {
			return RootResponse{}, err
		}
		binding, err := e.updateRootKubernetesBinding(request.Plan, joined)
		return RootResponse{NodeUID: joined.UID, NodeName: joined.Name, ResourceVersion: joined.ResourceVersion, KubernetesBinding: binding}, err
	case RootVerify:
		return RootResponse{}, e.verify(ctx, request.Plan, request.Join)
	default:
		return RootResponse{}, errors.New("root helper operation is unsupported")
	}
}
func (e NativeRootEngine) serviceState(ctx context.Context, service client.NodeInstallService) (RootResponse, error) {
	state := ServicePriorState{}
	if service.Manager == "systemd" {
		if output, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "is-enabled", service.UnitName); err == nil && strings.TrimSpace(string(output)) == "enabled" {
			state.Enabled = true
		}
		if output, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "is-active", service.UnitName); err == nil && strings.TrimSpace(string(output)) == "active" {
			state.Active = true
		}
	} else {
		if _, err := e.Commands.Run(ctx, "/bin/launchctl", "print", "system/"+service.UnitName); err == nil {
			state.Enabled = true
			state.Active = true
		}
	}
	return RootResponse{Service: &state}, nil
}
func (e NativeRootEngine) capture(ctx context.Context, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, backupRoot string) (PriorState, error) {
	if !canonicalPath(backupRoot) {
		return PriorState{}, errors.New("backup root is unsafe")
	}
	if (mutation.Kind == "systemd_unit" || mutation.Kind == "launchd_unit") && mutation.Action == "enable" {
		response, err := e.serviceState(ctx, plan.NodeService)
		if err != nil || response.Service == nil {
			return PriorState{}, errors.New("service state cannot be captured exactly")
		}
		return e.backupMetadata(backupRoot, plan, mutation, map[string]string{"kind": "service", "manager": plan.NodeService.Manager, "name": plan.NodeService.UnitName, "enabled": strconv.FormatBool(response.Service.Enabled), "active": strconv.FormatBool(response.Service.Active)})
	}
	if mutation.Kind == "group" {
		group, err := user.LookupGroup(mutation.Target)
		if err != nil {
			var unknown user.UnknownGroupError
			if errors.As(err, &unknown) {
				return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
			}
			return PriorState{}, err
		}
		return e.backupMetadata(backupRoot, plan, mutation, map[string]string{"kind": "group", "name": group.Name, "gid": group.Gid})
	}
	if mutation.Kind == "user" {
		account, err := user.Lookup(mutation.Target)
		if err != nil {
			var unknown user.UnknownUserError
			if errors.As(err, &unknown) {
				return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
			}
			return PriorState{}, err
		}
		output, err := e.Commands.Run(ctx, "/usr/bin/getent", "passwd", mutation.Target)
		parts := strings.Split(strings.TrimSpace(string(output)), ":")
		if err != nil || len(parts) != 7 || parts[0] != account.Username || parts[2] != account.Uid || parts[3] != account.Gid || parts[5] != account.HomeDir {
			return PriorState{}, errors.New("preexisting user state cannot be captured exactly")
		}
		return e.backupMetadata(backupRoot, plan, mutation, map[string]string{"kind": "user", "name": account.Username, "uid": account.Uid, "gid": account.Gid, "home": account.HomeDir, "shell": parts[6]})
	}
	if mutation.Kind == "package" {
		manager := stringValue(mutation.Desired["manager"])
		version, err := e.installedPackageVersion(ctx, mutation.Target, manager)
		if err != nil {
			var commandError *FixedCommandError
			if errors.As(err, &commandError) && commandError.ExitCode == 1 {
				return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
			}
			return PriorState{}, err
		}
		return e.backupMetadata(backupRoot, plan, mutation, map[string]string{"kind": "package", "name": mutation.Target, "manager": manager, "version": version})
	}
	if mutation.Kind == "directory" {
		info, err := os.Lstat(mutation.Target)
		if errors.Is(err, os.ErrNotExist) {
			return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return PriorState{}, errors.New("preexisting directory state is unsafe")
		}
		uid, _, ownerOK := fileOwner(info)
		gid, groupOK := fileGroup(info)
		if !ownerOK || !groupOK {
			return PriorState{}, errors.New("preexisting directory ownership is unavailable")
		}
		return e.backupMetadata(backupRoot, plan, mutation, map[string]string{"kind": "directory", "mode": strconv.FormatUint(uint64(info.Mode().Perm()), 8), "uid": strconv.FormatInt(uid, 10), "gid": strconv.FormatInt(gid, 10)})
	}
	if strings.HasPrefix(mutation.Target, "/") {
		info, err := os.Lstat(mutation.Target)
		if errors.Is(err, os.ErrNotExist) {
			return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return PriorState{}, errors.New("mutation target is unsafe")
		}
		content := []byte{}
		if info.Mode().IsRegular() {
			content, err = readBoundedRegular(mutation.Target, 512<<20)
			if err != nil {
				return PriorState{}, err
			}
		}
		return e.backupFile(backupRoot, plan, mutation, info, content)
	}
	return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
}
func (e NativeRootEngine) backupMetadata(root string, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, value any) (PriorState, error) {
	encoded, _ := json.Marshal(value)
	return e.writeBackup(root, plan, mutation, encoded, "metadata_snapshot", 0600, 0, 0)
}
func (e NativeRootEngine) backupFile(root string, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, info os.FileInfo, content []byte) (PriorState, error) {
	owner, _, ok := fileOwner(info)
	group, groupOK := fileGroup(info)
	if !ok || !groupOK {
		return PriorState{}, errors.New("target owner is unavailable")
	}
	return e.writeBackup(root, plan, mutation, content, "file_backup", info.Mode().Perm(), owner, group)
}
func (e NativeRootEngine) writeBackup(root string, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, value []byte, kind string, mode os.FileMode, uid, gid int64) (PriorState, error) {
	id := backupID(plan.PlanID, mutation.Ordinal)
	if err := ensurePrivateDirectory(root, 0); err != nil {
		return PriorState{}, err
	}
	path := filepath.Join(root, id)
	if err := writeRootAtomic(path, value, 0600, 0, 0); err != nil {
		return PriorState{}, err
	}
	sum := sha256.Sum256(value)
	m, u, g := int64(mode.Perm()), uid, gid
	return PriorState{State: "preexisting_exact", Material: client.NodeRollbackMaterial{Kind: kind, Locator: "receipt-backup://" + id, Digest: "sha256:" + hex.EncodeToString(sum[:]), Mode: &m, UID: &u, GID: &g}}, nil
}

func (e NativeRootEngine) apply(ctx context.Context, plan client.NodeInstallPlan, m client.NodeInstallMutation, material *RootMaterial, join *RootJoinBinding) error {
	switch m.Kind {
	case "group":
		gid := number(m.Desired["gid"])
		if existing, err := user.LookupGroup(m.Target); err == nil {
			if existing.Gid != strconv.FormatInt(gid, 10) {
				return errors.New("existing group differs from exact signed GID")
			}
			return nil
		}
		_, err := e.Commands.Run(ctx, "/usr/sbin/groupadd", "--system", "--gid", strconv.FormatInt(gid, 10), m.Target)
		return err
	case "user":
		if _, err := user.Lookup(m.Target); err == nil {
			return e.verifyUser(ctx, m)
		}
		_, err := e.Commands.Run(ctx, "/usr/sbin/useradd", "--system", "--uid", strconv.FormatInt(number(m.Desired["uid"]), 10), "--gid", stringValue(m.Desired["group"]), "--home-dir", stringValue(m.Desired["home"]), "--shell", stringValue(m.Desired["shell"]), m.Target)
		return err
	case "directory":
		if err := verifyNoSymlinkTraversal(m.Target); err != nil {
			return err
		}
		if m.Action == "adopt_exact" {
			return verifyExactDirectory(m.Target, os.FileMode(m.Mode), m.UID, m.GID)
		}
		if err := os.MkdirAll(m.Target, os.FileMode(m.Mode)); err != nil {
			return err
		}
		if err := os.Chown(m.Target, int(m.UID), int(m.GID)); err != nil {
			return err
		}
		return os.Chmod(m.Target, os.FileMode(m.Mode))
	case "file", "certificate":
		if component := planComponent(plan, material); component != nil && material.ContentBase64 == "" {
			if m.Action != "adopt_exact" || (component.SourceClass != "current_binary" && component.SourceClass != "embedded") {
				return errors.New("digest-only adopted material binding is invalid")
			}
			if component.SourceClass == "current_binary" && component.ArtifactType != "binary" {
				return errors.New("current binary mutation binding is invalid")
			}
			return verifyFileDigestAndMetadata(m.Target, component.SHA256, os.FileMode(m.Mode), m.UID, m.GID)
		}
		content, err := decodeMaterial(material)
		if err != nil {
			return err
		}
		if m.Action == "adopt_exact" {
			return verifyExactFile(m.Target, content, os.FileMode(m.Mode), m.UID, m.GID)
		}
		return writeRootAtomic(m.Target, content, os.FileMode(m.Mode), int(m.UID), int(m.GID))
	case "systemd_unit", "launchd_unit":
		if m.Action == "enable" {
			if m.Kind == "systemd_unit" {
				_, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "enable", "--now", plan.NodeService.UnitName)
				return err
			}
			if _, err := e.Commands.Run(ctx, "/bin/launchctl", "print", "system/"+plan.NodeService.UnitName); err == nil {
				return nil
			}
			_, err := e.Commands.Run(ctx, "/bin/launchctl", "bootstrap", "system", m.Target)
			return err
		}
		content, err := decodeMaterial(material)
		if err != nil {
			return err
		}
		if err := writeRootAtomic(m.Target, content, os.FileMode(m.Mode), int(m.UID), int(m.GID)); err != nil {
			return err
		}
		if m.Kind == "systemd_unit" {
			_, err = e.Commands.Run(ctx, "/usr/bin/systemctl", "daemon-reload")
		}
		return err
	case "package":
		manager := stringValue(m.Desired["manager"])
		if m.Action == "adopt_exact" {
			return e.verifyPackage(ctx, m, manager)
		}
		name, cleanup, err := e.stageHTTPSPackage(ctx, plan, m, material)
		if err != nil {
			return err
		}
		defer cleanup()
		if manager == "snap" {
			_, err = e.Commands.Run(ctx, "/usr/bin/snap", "install", name, "--dangerous")
		} else if manager == "brew" {
			_, err = e.Commands.Run(ctx, "/opt/homebrew/bin/brew", "install", name)
		} else {
			_, err = e.Commands.Run(ctx, "/usr/bin/apt-get", "install", "-y", name)
		}
		return err
	case "image":
		_, err := e.Commands.Run(ctx, "/snap/bin/microk8s.ctr", "images", "pull", m.Target)
		return err
	case "label":
		return e.applyClusterMutation(ctx, plan, m, join, false)
	case "taint":
		return e.applyClusterMutation(ctx, plan, m, join, false)
	case "firewall":
		_, err := e.Commands.Run(ctx, "/usr/sbin/ufw", "allow", strconv.FormatInt(number(m.Desired["port"]), 10)+"/"+stringValue(m.Desired["protocol"]))
		return err
	default:
		return errors.New("mutation kind is unsupported")
	}
}

func planComponent(plan client.NodeInstallPlan, material *RootMaterial) *client.NodeInstallComponent {
	if material == nil {
		return nil
	}
	for index := range plan.Components {
		if plan.Components[index].Name == material.ComponentName && plan.Components[index].SHA256 == material.SHA256 {
			return &plan.Components[index]
		}
	}
	return nil
}
func (e NativeRootEngine) verifyPackage(ctx context.Context, m client.NodeInstallMutation, manager string) error {
	expected := stringValue(m.Desired["version"])
	installed, err := e.installedPackageVersion(ctx, m.Target, manager)
	if err != nil {
		return err
	}
	if manager == "snap" {
		revision := strings.TrimPrefix(expected, "v1.35.6-rev")
		if revision == expected || installed != "revision:"+revision {
			return errors.New("installed snap revision differs from signed plan")
		}
		return nil
	}
	if installed != expected {
		if manager == "brew" {
			return errors.New("installed brew version differs from signed plan")
		}
		return errors.New("installed package version differs from signed plan")
	}
	return nil
}

func (e NativeRootEngine) installedPackageVersion(ctx context.Context, target, manager string) (string, error) {
	if manager == "snap" {
		output, err := e.Commands.Run(ctx, "/usr/bin/snap", "list", target, "--unicode=never")
		if err != nil {
			return "", err
		}
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		fields := strings.Fields(lines[len(lines)-1])
		if len(fields) < 3 || fields[0] != target {
			return "", errors.New("installed snap response is invalid")
		}
		return "revision:" + fields[2], nil
	}
	if manager == "brew" {
		output, err := e.Commands.Run(ctx, "/opt/homebrew/bin/brew", "list", "--versions", target)
		if err != nil {
			return "", err
		}
		fields := strings.Fields(string(output))
		if len(fields) != 2 || fields[0] != target {
			return "", errors.New("installed brew response is invalid")
		}
		return fields[1], nil
	}
	if manager != "apt" {
		return "", errors.New("package manager is unsupported")
	}
	output, err := e.Commands.Run(ctx, "/usr/bin/dpkg-query", "-W", "-f=${Version}", target)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
func (e NativeRootEngine) verifyUser(ctx context.Context, m client.NodeInstallMutation) error {
	output, err := e.Commands.Run(ctx, "/usr/bin/getent", "passwd", m.Target)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimSpace(string(output)), ":")
	if len(parts) != 7 || parts[0] != m.Target || parts[2] != strconv.FormatInt(number(m.Desired["uid"]), 10) || parts[3] != strconv.FormatInt(number(m.Desired["gid"]), 10) || parts[5] != stringValue(m.Desired["home"]) || parts[6] != stringValue(m.Desired["shell"]) {
		return errors.New("existing user differs from exact signed account")
	}
	return nil
}
func verifyExactFile(path string, content []byte, mode os.FileMode, uid, gid int64) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode.Perm() {
		return errors.New("adopted file metadata differs from signed plan")
	}
	owner, _, ok := fileOwner(info)
	group, groupOK := fileGroup(info)
	if !ok || !groupOK || owner != uid || group != gid {
		return errors.New("adopted file ownership differs from signed plan")
	}
	value, err := readBoundedRegular(path, 512<<20)
	if err != nil {
		return err
	}
	if !bytes.Equal(value, content) {
		return errors.New("adopted file content differs from signed plan")
	}
	return nil
}
func verifyExactDirectory(path string, mode os.FileMode, uid, gid int64) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode.Perm() {
		return errors.New("adopted directory metadata differs from signed plan")
	}
	owner, _, ok := fileOwner(info)
	group, groupOK := fileGroup(info)
	if !ok || !groupOK || owner != uid || group != gid {
		return errors.New("adopted directory ownership differs from signed plan")
	}
	return nil
}
func (e NativeRootEngine) rollback(ctx context.Context, plan client.NodeInstallPlan, m client.NodeInstallMutation, prior PriorState, backupRoot string, join *RootJoinBinding) error {
	if prior.State == "absent" {
		switch m.Kind {
		case "group":
			_, err := e.Commands.Run(ctx, "/usr/sbin/groupdel", m.Target)
			return err
		case "user":
			_, err := e.Commands.Run(ctx, "/usr/sbin/userdel", m.Target)
			return err
		case "systemd_unit":
			return os.Remove(m.Target)
		case "launchd_unit":
			return os.Remove(m.Target)
		case "package":
			manager := stringValue(m.Desired["manager"])
			if manager == "snap" {
				_, err := e.Commands.Run(ctx, "/usr/bin/snap", "remove", m.Target)
				return err
			}
			if manager == "brew" {
				_, err := e.Commands.Run(ctx, "/opt/homebrew/bin/brew", "uninstall", m.Target)
				return err
			}
			_, err := e.Commands.Run(ctx, "/usr/bin/apt-get", "remove", "-y", m.Target)
			return err
		case "label":
			return e.applyClusterMutation(ctx, plan, m, join, true)
		case "taint":
			return e.applyClusterMutation(ctx, plan, m, join, true)
		default:
			if strings.HasPrefix(m.Target, "/") {
				return os.Remove(m.Target)
			}
		}
		return nil
	}
	if prior.Material.Kind == "file_backup" {
		if prior.Material.Mode == nil || prior.Material.UID == nil || prior.Material.GID == nil {
			return errors.New("file rollback metadata is incomplete")
		}
		path, err := client.ResolveNodeRollbackLocator(backupRoot, prior.Material.Locator)
		if err != nil {
			return err
		}
		content, err := readBoundedRegular(path, 512<<20)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		if "sha256:"+hex.EncodeToString(sum[:]) != prior.Material.Digest {
			return errors.New("rollback backup digest mismatch")
		}
		return writeRootAtomic(m.Target, content, os.FileMode(*prior.Material.Mode), int(*prior.Material.UID), int(*prior.Material.GID))
	}
	if prior.Material.Kind == "metadata_snapshot" {
		path, err := client.ResolveNodeRollbackLocator(backupRoot, prior.Material.Locator)
		if err != nil {
			return err
		}
		content, err := readBoundedRegular(path, 1<<20)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		if "sha256:"+hex.EncodeToString(sum[:]) != prior.Material.Digest {
			return errors.New("rollback metadata digest mismatch")
		}
		var metadata map[string]string
		if json.Unmarshal(content, &metadata) != nil {
			return errors.New("rollback metadata is invalid")
		}
		switch metadata["kind"] {
		case "group":
			_, err = e.Commands.Run(ctx, "/usr/sbin/groupmod", "--gid", metadata["gid"], m.Target)
			return err
		case "user":
			args := []string{"--uid", metadata["uid"], "--gid", metadata["gid"], "--home", metadata["home"], "--shell", metadata["shell"], m.Target}
			_, err = e.Commands.Run(ctx, "/usr/sbin/usermod", args...)
			return err
		case "package":
			manager := metadata["manager"]
			if manager == "snap" {
				_, err = e.Commands.Run(ctx, "/usr/bin/snap", "refresh", m.Target, "--revision", strings.TrimPrefix(metadata["version"], "revision:"))
				return err
			}
			if manager == "brew" {
				_, err = e.Commands.Run(ctx, "/opt/homebrew/bin/brew", "install", m.Target+"@"+metadata["version"])
				return err
			}
			_, err = e.Commands.Run(ctx, "/usr/bin/apt-get", "install", "-y", m.Target+"="+metadata["version"])
			return err
		case "service":
			enabled, enabledErr := strconv.ParseBool(metadata["enabled"])
			active, activeErr := strconv.ParseBool(metadata["active"])
			if enabledErr != nil || activeErr != nil || metadata["name"] != plan.NodeService.UnitName || metadata["manager"] != plan.NodeService.Manager {
				return errors.New("rollback service metadata is invalid")
			}
			if metadata["manager"] == "systemd" {
				enableAction := "disable"
				if enabled {
					enableAction = "enable"
				}
				if _, err = e.Commands.Run(ctx, "/usr/bin/systemctl", enableAction, metadata["name"]); err != nil {
					return err
				}
				activeAction := "stop"
				if active {
					activeAction = "start"
				}
				_, err = e.Commands.Run(ctx, "/usr/bin/systemctl", activeAction, metadata["name"])
				return err
			}
			_, currentErr := e.Commands.Run(ctx, "/bin/launchctl", "print", "system/"+metadata["name"])
			if active || enabled {
				if currentErr == nil {
					return nil
				}
				_, err = e.Commands.Run(ctx, "/bin/launchctl", "bootstrap", "system", m.Target)
			} else {
				if currentErr != nil {
					return nil
				}
				_, err = e.Commands.Run(ctx, "/bin/launchctl", "bootout", "system/"+metadata["name"])
			}
			return err
		case "directory":
			return restoreDirectoryMetadata(m.Target, metadata)
		default:
			return errors.New("rollback metadata kind is unsupported")
		}
	}
	return errors.New("rollback material kind is unsupported")
}

func restoreDirectoryMetadata(target string, metadata map[string]string) error {
	mode, modeErr := strconv.ParseUint(metadata["mode"], 8, 32)
	uid, uidErr := strconv.ParseInt(metadata["uid"], 10, 32)
	gid, gidErr := strconv.ParseInt(metadata["gid"], 10, 32)
	if metadata["kind"] != "directory" || modeErr != nil || uidErr != nil || gidErr != nil || !canonicalPath(target) {
		return errors.New("rollback directory metadata is invalid")
	}
	if err := verifyNoSymlinkTraversal(target); err != nil {
		return err
	}
	if err := os.Chown(target, int(uid), int(gid)); err != nil {
		return err
	}
	return os.Chmod(target, os.FileMode(mode))
}

func (e NativeRootEngine) join(ctx context.Context, plan client.NodeInstallPlan, binding *RootJoinBinding) (JoinedNode, error) {
	if binding == nil || !binding.WorkerOnly || binding.ClusterID != plan.Cluster.ID {
		return JoinedNode{}, errors.New("join binding is invalid")
	}
	payload, urls, err := decodeJoinCredential(binding.Credential, binding)
	if err != nil {
		return JoinedNode{}, err
	}
	_ = payload
	if len(urls) == 0 {
		return JoinedNode{}, errors.New("join credential has no endpoint")
	}
	if err := e.verifyJoinRuntime(ctx, plan); err != nil {
		return JoinedNode{}, err
	}
	input := []byte(urls[0] + "\n")
	if e.Platform == "linux" {
		if _, err := e.Commands.RunInput(ctx, "/snap/microk8s/current/usr/bin/python3", input, "-c", microK8sJoinStdinProgram); err != nil {
			return JoinedNode{}, err
		}
	} else {
		vm, err := readLimaVM(plan, e.LimaBindingPath)
		if err != nil {
			return JoinedNode{}, err
		}
		if _, err := e.Commands.RunInput(ctx, "/usr/local/bin/limactl", input, "shell", vm, "sudo", "/usr/bin/env", "SNAP=/snap/microk8s/current", "SNAP_DATA=/var/snap/microk8s/current", "SNAP_COMMON=/var/snap/microk8s/common", "/snap/microk8s/current/usr/bin/python3", "-c", microK8sJoinStdinProgram); err != nil {
			return JoinedNode{}, err
		}
	}
	return e.observeNode(ctx, plan, binding.ExpectedNodeName)
}

const microK8sJoinStdinProgram = `import importlib.util,sys
p="/snap/microk8s/current/scripts/wrappers/join.py"
s=importlib.util.spec_from_file_location("blazn_microk8s_join",p)
m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
c=sys.stdin.readline().strip()
if not c: raise SystemExit(2)
m.join.callback(c,"as-worker",False,False)
`
const pinnedJoinSHA256 = "cd050fe6926af0ae07a7505834eb92a94c1f370f89e7cdcedb79e95ed63ad419"

func (e NativeRootEngine) verifyJoinRuntime(ctx context.Context, plan client.NodeInstallPlan) error {
	if e.allowTestJoinRuntime {
		return nil
	}
	if e.Platform == "linux" {
		value, err := os.ReadFile("/snap/microk8s/current/scripts/wrappers/join.py")
		if err != nil {
			return err
		}
		sum := sha256.Sum256(value)
		if hex.EncodeToString(sum[:]) != pinnedJoinSHA256 {
			return errors.New("pinned MicroK8s join helper digest mismatch")
		}
		return nil
	}
	vm, err := readLimaVM(plan, e.LimaBindingPath)
	if err != nil {
		return err
	}
	output, err := e.Commands.Run(ctx, "/usr/local/bin/limactl", "shell", vm, "sha256sum", "/snap/microk8s/current/scripts/wrappers/join.py")
	if err != nil || !strings.HasPrefix(string(output), pinnedJoinSHA256+" ") {
		return errors.New("Lima MicroK8s join helper digest mismatch")
	}
	return nil
}
func (e NativeRootEngine) kubectl(ctx context.Context, plan client.NodeInstallPlan, args ...string) ([]byte, error) {
	if e.Platform == "linux" {
		return e.Commands.Run(ctx, "/snap/bin/microk8s.kubectl", args...)
	}
	vm, err := readLimaVM(plan, e.LimaBindingPath)
	if err != nil {
		return nil, err
	}
	fixed := append([]string{"shell", vm, "sudo", "/snap/bin/microk8s.kubectl"}, args...)
	return e.Commands.Run(ctx, "/usr/local/bin/limactl", fixed...)
}
func (e NativeRootEngine) applyClusterMutation(ctx context.Context, plan client.NodeInstallPlan, m client.NodeInstallMutation, join *RootJoinBinding, remove bool) error {
	if join == nil || join.ExpectedNodeUID == "" {
		return errors.New("cluster mutation requires joined node binding")
	}
	observed, err := e.observeNode(ctx, plan, join.ExpectedNodeName)
	if err != nil || observed.UID != join.ExpectedNodeUID {
		return errors.New("cluster mutation node UID differs from binding")
	}
	if join.ExpectedResourceVersion == "" || observed.ResourceVersion != join.ExpectedResourceVersion {
		return errors.New("cluster mutation resourceVersion differs from its precondition")
	}
	var args []string
	if m.Kind == "label" {
		value := m.Target + "=" + stringValue(m.Desired["value"])
		if remove {
			value = m.Target + "-"
		}
		args = []string{"label", "node", join.ExpectedNodeName, value, "--overwrite"}
	} else {
		value := m.Target + "=" + stringValue(m.Desired["value"]) + ":" + stringValue(m.Desired["effect"])
		if remove {
			value = m.Target + ":" + stringValue(m.Desired["effect"]) + "-"
		}
		args = []string{"taint", "node", join.ExpectedNodeName, value, "--overwrite"}
	}
	_, err = e.kubectl(ctx, plan, args...)
	return err
}
func (e NativeRootEngine) observeNode(ctx context.Context, plan client.NodeInstallPlan, name string) (JoinedNode, error) {
	output, err := e.kubectl(ctx, plan, "get", "node", name, "-o", "json")
	if err != nil {
		return JoinedNode{}, err
	}
	var value struct {
		Metadata struct {
			Name            string `json:"name"`
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if json.Unmarshal(output, &value) != nil || value.Metadata.UID == "" || value.Metadata.Name != name || value.Metadata.ResourceVersion == "" {
		return JoinedNode{}, errors.New("joined node binding is unavailable")
	}
	return JoinedNode{Name: value.Metadata.Name, UID: value.Metadata.UID, ResourceVersion: value.Metadata.ResourceVersion}, nil
}
func (e NativeRootEngine) verify(ctx context.Context, plan client.NodeInstallPlan, binding *RootJoinBinding) error {
	if binding == nil || binding.ExpectedNodeUID == "" {
		return errors.New("verification binding is incomplete")
	}
	joined, err := e.observeNode(ctx, plan, binding.ExpectedNodeName)
	if err != nil || joined.UID != binding.ExpectedNodeUID {
		return errors.New("joined node UID differs from binding")
	}
	output, err := e.kubectl(ctx, plan, "get", "node", binding.ExpectedNodeName, "-o", "json")
	if err != nil {
		return err
	}
	var node struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Taints []struct {
				Key    string `json:"key"`
				Value  string `json:"value"`
				Effect string `json:"effect"`
			} `json:"taints"`
		} `json:"spec"`
	}
	if json.Unmarshal(output, &node) != nil {
		return errors.New("joined node taint response is invalid")
	}
	for _, mutation := range sortedMutations(plan) {
		if err := e.verifyMutation(ctx, plan, mutation, node.Metadata.Labels, node.Spec.Taints); err != nil {
			return fmt.Errorf("verify mutation %d: %w", mutation.Ordinal, err)
		}
	}
	observedTaint := false
	for _, taint := range node.Spec.Taints {
		if taint.Key == "blazn.dev/bootstrap" && taint.Value == "pending" && taint.Effect == "NoSchedule" {
			observedTaint = true
		}
	}
	if !observedTaint {
		return errors.New("bootstrap taint is not observed")
	}
	return nil
}

func (e NativeRootEngine) verifyMutation(ctx context.Context, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, labels map[string]string, taints []struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}) error {
	switch mutation.Kind {
	case "group":
		group, err := user.LookupGroup(mutation.Target)
		if err != nil || group.Gid != strconv.FormatInt(number(mutation.Desired["gid"]), 10) {
			return errors.New("group differs from signed state")
		}
		return nil
	case "user":
		return e.verifyUser(ctx, mutation)
	case "directory":
		return verifyExactDirectory(mutation.Target, os.FileMode(mutation.Mode), mutation.UID, mutation.GID)
	case "file", "certificate":
		return verifyFileDigestAndMetadata(mutation.Target, stringValue(mutation.Desired["contentSha256"]), os.FileMode(mutation.Mode), mutation.UID, mutation.GID)
	case "systemd_unit", "launchd_unit":
		if mutation.Action != "enable" {
			return verifyFileDigestAndMetadata(mutation.Target, componentDigest(plan, stringValue(mutation.Desired["sourceComponent"])), os.FileMode(mutation.Mode), mutation.UID, mutation.GID)
		}
		if mutation.Kind == "systemd_unit" {
			enabled, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "is-enabled", plan.NodeService.UnitName)
			if err != nil || strings.TrimSpace(string(enabled)) != "enabled" {
				return errors.New("systemd service is not enabled")
			}
			active, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "is-active", plan.NodeService.UnitName)
			if err != nil || strings.TrimSpace(string(active)) != "active" {
				return errors.New("systemd service is not active")
			}
			return nil
		}
		_, err := e.Commands.Run(ctx, "/bin/launchctl", "print", "system/"+plan.NodeService.UnitName)
		return err
	case "package":
		return e.verifyPackage(ctx, mutation, stringValue(mutation.Desired["manager"]))
	case "image":
		_, err := e.Commands.Run(ctx, "/snap/bin/microk8s.ctr", "images", "inspect", mutation.Target)
		return err
	case "label":
		if labels[mutation.Target] != stringValue(mutation.Desired["value"]) {
			return errors.New("node label differs from signed state")
		}
		return nil
	case "taint":
		for _, taint := range taints {
			if taint.Key == mutation.Target && taint.Value == stringValue(mutation.Desired["value"]) && taint.Effect == stringValue(mutation.Desired["effect"]) {
				return nil
			}
		}
		return errors.New("node taint differs from signed state")
	default:
		return errors.New("mutation verification is unsupported")
	}
}

func componentDigest(plan client.NodeInstallPlan, name string) string {
	for _, component := range plan.Components {
		if component.Name == name {
			return component.SHA256
		}
	}
	return ""
}

func verifyFileDigestAndMetadata(path, expectedDigest string, mode os.FileMode, uid, gid int64) error {
	if expectedDigest == "" {
		return errors.New("signed file digest is unavailable")
	}
	value, err := readBoundedRegular(path, 512<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(value)
	if hex.EncodeToString(sum[:]) != expectedDigest {
		return errors.New("file digest differs from signed state")
	}
	return verifyExactFile(path, value, mode, uid, gid)
}

type joinPayload struct {
	SchemaVersion    string    `json:"schemaVersion"`
	IssuanceID       string    `json:"issuanceId"`
	ClusterID        string    `json:"clusterId"`
	ExpectedNodeName string    `json:"expectedNodeName"`
	BootstrapTaint   string    `json:"bootstrapTaint"`
	WorkerOnly       bool      `json:"workerOnly"`
	ExpiresAt        time.Time `json:"expiresAt"`
	URLs             []string  `json:"urls"`
}

func decodeJoinCredential(encoded string, binding *RootJoinBinding) (joinPayload, []string, error) {
	var payload joinPayload
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return payload, nil, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 8 {
		return payload, nil, errors.New("worker credential payload fields are invalid")
	}
	for _, key := range []string{"schemaVersion", "issuanceId", "clusterId", "expectedNodeName", "bootstrapTaint", "workerOnly", "expiresAt", "urls"} {
		if fields[key] == nil {
			return payload, nil, errors.New("worker credential payload fields are incomplete")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, nil, err
	}
	if payload.SchemaVersion != "blazn.dev/microk8s-worker-join/v1" || payload.IssuanceID == "" || payload.ClusterID != binding.ClusterID || payload.ExpectedNodeName != binding.ExpectedNodeName || payload.BootstrapTaint != binding.BootstrapTaint || !payload.WorkerOnly || !time.Now().Before(payload.ExpiresAt) {
		return payload, nil, errors.New("worker credential payload is invalid")
	}
	for _, candidate := range payload.URLs {
		parsed, err := url.Parse("https://" + candidate)
		parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
		if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || net.ParseIP(parsed.Hostname()) == nil || parsed.Port() == "" || len(parts) != 2 || len(parts[0]) != 32 || len(parts[1]) < 16 {
			return payload, nil, errors.New("worker credential endpoint is invalid")
		}
	}
	return payload, payload.URLs, nil
}
func readLimaVM(plan client.NodeInstallPlan, configuredPath string) (string, error) {
	var expected string
	for _, component := range plan.Components {
		if component.Name == "lima-worker-binding" && component.SourceClass == "embedded" && component.ArtifactType == "configuration" {
			expected = component.SHA256
			break
		}
	}
	if expected == "" {
		return "", errors.New("Lima binding component is unavailable")
	}
	path := configuredPath
	if path == "" {
		path = "/Library/Application Support/Blazn/lima-worker-binding.json"
	}
	value, err := readBoundedRegular(path, 64<<10)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(value)
	if hex.EncodeToString(sum[:]) != expected {
		return "", errors.New("Lima binding digest differs from signed component")
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(value, &raw) != nil || len(raw) != 4 {
		return "", errors.New("Lima binding is invalid")
	}
	var binding struct {
		SchemaVersion string `json:"schemaVersion"`
		ClusterID     string `json:"clusterId"`
		VMName        string `json:"vmName"`
		WorkerName    string `json:"workerName"`
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&binding) != nil || binding.SchemaVersion != "blazn.dev/lima-worker-binding/v1" || binding.ClusterID != plan.Cluster.ID || binding.WorkerName != plan.Hostname || binding.VMName == "" {
		return "", errors.New("Lima binding does not match signed plan")
	}
	return binding.VMName, nil
}
func decodeMaterial(material *RootMaterial) ([]byte, error) {
	if material == nil {
		return nil, errors.New("mutation material is missing")
	}
	value, err := base64.RawStdEncoding.DecodeString(material.ContentBase64)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(value)
	if hex.EncodeToString(sum[:]) != material.SHA256 {
		return nil, errors.New("mutation material digest mismatch")
	}
	return value, nil
}
func writeRootAtomic(path string, value []byte, mode os.FileMode, uid, gid int) error {
	if !canonicalPath(path) {
		return errors.New("root write path is unsafe")
	}
	if err := verifyNoSymlinkTraversal(path); err != nil {
		return err
	}
	return writeRootAtomicNative(path, value, mode, uid, gid)
}
func backupID(planID string, ordinal int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", planID, ordinal)))
	value := sum[:16]
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}
func number(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}
func stringValue(value any) string { result, _ := value.(string); return result }
