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
	allowed := map[string]bool{"schemaVersion": true, "operation": true, "platform": true, "plan": true, "ordinal": true, "backupRoot": true, "prior": true, "material": true, "join": true}
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
	if request.Operation != RootProbe {
		if err := client.ValidateNodeInstallPlan(request.Plan); err != nil {
			return err
		}
		if (request.Platform == "linux" && request.Plan.Target.Platform != client.NodePlatformLinux) || (request.Platform == "macos" && request.Plan.Target.Platform != client.NodePlatformMacOS) {
			return errors.New("root helper plan platform mismatch")
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

type CommandExecutor interface {
	Run(context.Context, string, ...string) ([]byte, error)
}
type FixedCommandExecutor struct{}

func (FixedCommandExecutor) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, path, args...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/bin", "LANG=C", "LC_ALL=C"}
	var stdout bytes.Buffer
	command.Stdout = &limitedOutput{writer: &stdout, remaining: 1 << 20}
	command.Stderr = &limitedOutput{writer: &bytes.Buffer{}, remaining: 4096}
	if err := command.Run(); err != nil {
		return nil, errors.New("fixed privileged command failed")
	}
	return stdout.Bytes(), nil
}

type NativeRootEngine struct {
	Platform string
	Commands CommandExecutor
	Now      func() time.Time
}

func (e NativeRootEngine) Execute(ctx context.Context, request RootRequest) (RootResponse, error) {
	if e.Commands == nil {
		e.Commands = FixedCommandExecutor{}
	}
	switch request.Operation {
	case RootProbe:
		if e.Platform != request.Platform {
			return RootResponse{}, errors.New("root helper OS mismatch")
		}
		return RootResponse{}, nil
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
		return RootResponse{}, e.apply(ctx, request.Plan, mutation, request.Material, request.Join)
	case RootRollback:
		mutation, err := mutationByOrdinal(request.Plan, request.Ordinal)
		if err != nil || request.Prior == nil {
			return RootResponse{}, errors.New("rollback request is incomplete")
		}
		return RootResponse{}, e.rollback(ctx, mutation, *request.Prior)
	case RootJoin:
		uid, err := e.join(ctx, request.Plan, request.Join)
		return RootResponse{NodeUID: uid}, err
	case RootVerify:
		return RootResponse{}, e.verify(ctx, request.Plan, request.Join)
	default:
		return RootResponse{}, errors.New("root helper operation is unsupported")
	}
}
func (e NativeRootEngine) serviceState(ctx context.Context, service client.NodeInstallService) (RootResponse, error) {
	state := ServicePriorState{}
	if service.Manager == "systemd" {
		if _, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "is-enabled", service.UnitName); err == nil {
			state.Enabled = true
		}
		if _, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "is-active", service.UnitName); err == nil {
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
	if mutation.Kind == "group" || mutation.Kind == "user" {
		account, err := user.Lookup(mutation.Target)
		if err != nil {
			return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
		}
		return e.backupMetadata(backupRoot, plan, mutation, map[string]string{"name": account.Username, "uid": account.Uid, "gid": account.Gid, "home": account.HomeDir})
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
	if !ok {
		return PriorState{}, errors.New("target owner is unavailable")
	}
	return e.writeBackup(root, plan, mutation, content, "file_backup", info.Mode().Perm(), owner, owner)
}
func (e NativeRootEngine) writeBackup(root string, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, value []byte, kind string, mode os.FileMode, uid, gid int64) (PriorState, error) {
	id := backupID(plan.PlanID, mutation.Ordinal)
	if err := os.MkdirAll(root, 0700); err != nil {
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
		if _, err := user.LookupGroup(m.Target); err == nil {
			return nil
		}
		_, err := e.Commands.Run(ctx, "/usr/sbin/groupadd", "--system", "--gid", strconv.FormatInt(gid, 10), m.Target)
		return err
	case "user":
		if _, err := user.Lookup(m.Target); err == nil {
			return nil
		}
		_, err := e.Commands.Run(ctx, "/usr/sbin/useradd", "--system", "--uid", strconv.FormatInt(number(m.Desired["uid"]), 10), "--gid", stringValue(m.Desired["group"]), "--home-dir", stringValue(m.Desired["home"]), "--shell", stringValue(m.Desired["shell"]), m.Target)
		return err
	case "directory":
		if err := verifyNoSymlinkTraversal(m.Target); err != nil {
			return err
		}
		if err := os.MkdirAll(m.Target, os.FileMode(m.Mode)); err != nil {
			return err
		}
		if err := os.Chown(m.Target, int(m.UID), int(m.GID)); err != nil {
			return err
		}
		return os.Chmod(m.Target, os.FileMode(m.Mode))
	case "file", "certificate":
		content, err := decodeMaterial(material)
		if err != nil {
			return err
		}
		return writeRootAtomic(m.Target, content, os.FileMode(m.Mode), int(m.UID), int(m.GID))
	case "systemd_unit", "launchd_unit":
		if m.Action == "enable" {
			if m.Kind == "systemd_unit" {
				_, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "enable", "--now", plan.NodeService.UnitName)
				return err
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
		content, err := decodeMaterial(material)
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp("/var/tmp", "blazn-package-*")
		if err != nil {
			return err
		}
		name := tmp.Name()
		defer os.Remove(name)
		if err := tmp.Chmod(0600); err == nil {
			_, err = tmp.Write(content)
		}
		tmp.Close()
		if err != nil {
			return err
		}
		if manager == "snap" {
			_, err = e.Commands.Run(ctx, "/usr/bin/snap", "install", name, "--dangerous")
		} else if manager == "brew" {
			_, err = e.Commands.Run(ctx, "/opt/homebrew/bin/brew", "install", name)
		} else {
			_, err = e.Commands.Run(ctx, "/usr/bin/apt-get", "install", "-y", m.Target+"="+stringValue(m.Desired["version"]))
		}
		return err
	case "image":
		_, err := e.Commands.Run(ctx, "/snap/bin/microk8s.ctr", "images", "pull", m.Target)
		return err
	case "label":
		if join == nil || join.ExpectedNodeUID == "" {
			return errors.New("label requires joined node binding")
		}
		_, err := e.Commands.Run(ctx, "/snap/bin/microk8s.kubectl", "label", "node", join.ExpectedNodeName, m.Target+"="+stringValue(m.Desired["value"]), "--overwrite")
		return err
	case "taint":
		if join == nil || join.ExpectedNodeUID == "" {
			return errors.New("taint requires joined node binding")
		}
		_, err := e.Commands.Run(ctx, "/snap/bin/microk8s.kubectl", "taint", "node", join.ExpectedNodeName, m.Target+"="+stringValue(m.Desired["value"])+":"+stringValue(m.Desired["effect"]), "--overwrite")
		return err
	case "firewall":
		_, err := e.Commands.Run(ctx, "/usr/sbin/ufw", "allow", strconv.FormatInt(number(m.Desired["port"]), 10)+"/"+stringValue(m.Desired["protocol"]))
		return err
	default:
		return errors.New("mutation kind is unsupported")
	}
}
func (e NativeRootEngine) verifyPackage(ctx context.Context, m client.NodeInstallMutation, manager string) error {
	if manager == "snap" {
		_, err := e.Commands.Run(ctx, "/usr/bin/snap", "list", m.Target)
		return err
	}
	if manager == "brew" {
		_, err := e.Commands.Run(ctx, "/opt/homebrew/bin/brew", "list", "--versions", m.Target)
		return err
	}
	_, err := e.Commands.Run(ctx, "/usr/bin/dpkg-query", "-W", m.Target)
	return err
}
func (e NativeRootEngine) rollback(ctx context.Context, m client.NodeInstallMutation, prior PriorState) error {
	if prior.State == "absent" {
		switch m.Kind {
		case "group":
			_, err := e.Commands.Run(ctx, "/usr/sbin/groupdel", m.Target)
			return err
		case "user":
			_, err := e.Commands.Run(ctx, "/usr/sbin/userdel", m.Target)
			return err
		case "systemd_unit":
			_, _ = e.Commands.Run(ctx, "/usr/bin/systemctl", "disable", "--now", filepath.Base(m.Target))
			return os.Remove(m.Target)
		case "launchd_unit":
			_, _ = e.Commands.Run(ctx, "/bin/launchctl", "bootout", "system/"+strings.TrimSuffix(filepath.Base(m.Target), ".plist"))
			return os.Remove(m.Target)
		default:
			if strings.HasPrefix(m.Target, "/") {
				return os.RemoveAll(m.Target)
			}
		}
		return nil
	}
	if prior.Material.Kind == "file_backup" {
		path, err := client.ResolveNodeRollbackLocator(filepath.Dir(filepath.Dir(prior.Material.Locator)), prior.Material.Locator)
		_ = path
		_ = err
		return errors.New("file rollback requires receipt-bound backup resolver")
	}
	if prior.Material.Kind == "metadata_snapshot" {
		return nil
	}
	return errors.New("rollback material kind is unsupported")
}

func (e NativeRootEngine) join(ctx context.Context, plan client.NodeInstallPlan, binding *RootJoinBinding) (string, error) {
	if binding == nil || !binding.WorkerOnly || binding.ClusterID != plan.Cluster.ID {
		return "", errors.New("join binding is invalid")
	}
	payload, urls, err := decodeJoinCredential(binding.Credential, binding)
	if err != nil {
		return "", err
	}
	_ = payload
	if len(urls) == 0 {
		return "", errors.New("join credential has no endpoint")
	}
	if e.Platform == "linux" {
		if _, err := e.Commands.Run(ctx, "/snap/bin/microk8s.join", urls[0], "--worker"); err != nil {
			return "", err
		}
	} else {
		vm, err := readLimaVM(plan)
		if err != nil {
			return "", err
		}
		if _, err := e.Commands.Run(ctx, "/usr/local/bin/limactl", "shell", vm, "sudo", "/snap/bin/microk8s.join", urls[0], "--worker"); err != nil {
			return "", err
		}
	}
	return e.observeNodeUID(ctx, binding.ExpectedNodeName)
}
func (e NativeRootEngine) observeNodeUID(ctx context.Context, name string) (string, error) {
	output, err := e.Commands.Run(ctx, "/snap/bin/microk8s.kubectl", "get", "node", name, "-o", "json")
	if err != nil {
		return "", err
	}
	var value struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if json.Unmarshal(output, &value) != nil || value.Metadata.UID == "" {
		return "", errors.New("joined node UID is unavailable")
	}
	return value.Metadata.UID, nil
}
func (e NativeRootEngine) verify(ctx context.Context, plan client.NodeInstallPlan, binding *RootJoinBinding) error {
	if binding == nil || binding.ExpectedNodeUID == "" {
		return errors.New("verification binding is incomplete")
	}
	uid, err := e.observeNodeUID(ctx, binding.ExpectedNodeName)
	if err != nil || uid != binding.ExpectedNodeUID {
		return errors.New("joined node UID differs from binding")
	}
	output, err := e.Commands.Run(ctx, "/snap/bin/microk8s.kubectl", "get", "node", binding.ExpectedNodeName, "-o", "json")
	if err != nil {
		return err
	}
	if !bytes.Contains(output, []byte(plan.Cluster.BootstrapTaint)) {
		return errors.New("bootstrap taint is not observed")
	}
	return nil
}

type joinPayload struct {
	SchemaVersion, IssuanceID, ClusterID, ExpectedNodeName, BootstrapTaint string
	WorkerOnly                                                             bool
	ExpiresAt                                                              time.Time
	URLs                                                                   []string
}

func decodeJoinCredential(encoded string, binding *RootJoinBinding) (joinPayload, []string, error) {
	var payload joinPayload
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return payload, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, nil, err
	}
	if payload.SchemaVersion != "blazn.dev/microk8s-worker-join/v1" || payload.ClusterID != binding.ClusterID || payload.ExpectedNodeName != binding.ExpectedNodeName || payload.BootstrapTaint != binding.BootstrapTaint || !payload.WorkerOnly || !time.Now().Before(payload.ExpiresAt) {
		return payload, nil, errors.New("worker credential payload is invalid")
	}
	return payload, payload.URLs, nil
}
func readLimaVM(plan client.NodeInstallPlan) (string, error) {
	for _, component := range plan.Components {
		if component.Name == "lima-worker-binding" {
			return "blazn-worker", nil
		}
	}
	return "", errors.New("Lima binding component is unavailable")
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
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".blazn-root-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(mode); err == nil {
		err = tmp.Chown(uid, gid)
	}
	if err == nil {
		_, err = tmp.Write(value)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	return err
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
