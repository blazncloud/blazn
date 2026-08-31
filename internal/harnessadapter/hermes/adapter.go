package hermes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/blazncloud/blazn/internal/harnessworker"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

const (
	ReviewedExecutable = "/opt/blazn/hermes"
	RecordSchema       = "blazn.dev/hermes-adapter-record/v1alpha1"
	maxRecordBytes     = 128 << 10
	maxRecords         = 10000
	maxTokenBytes      = 4 << 10
	maxArtifactBytes   = 8 << 20
)

var ReviewedArgv = []string{"run", "--jsonl"}

type Config struct {
	ProxyURL     string
	Output       io.Writer
	ArtifactRoot string
	Now          func() time.Time
}

// Adapter is a single-run reference Hermes adapter. Process supervision stays
// in harnessworker; this type only seals the reviewed command and normalizes
// Hermes JSONL while the process is running.
type Adapter struct {
	proxyURL     string
	output       io.Writer
	artifactRoot string
	now          func() time.Time

	mu       sync.Mutex
	prepared bool
	normal   *normalizer
}

func New(config Config) (*Adapter, error) {
	proxyURL, err := validateProxyURL(config.ProxyURL)
	if err != nil {
		return nil, err
	}
	if config.Output == nil {
		return nil, errors.New("Hermes normalized output is required")
	}
	if config.ArtifactRoot == "" {
		config.ArtifactRoot = harnessworker.DefaultArtifactRoot
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Adapter{proxyURL: proxyURL, output: config.Output, artifactRoot: config.ArtifactRoot, now: config.Now}, nil
}

// Prepare implements harnessworker.Adapter. Assignment data cannot select the
// executable, arguments, environment, working directory, or inherited files.
func (a *Adapter) Prepare(ctx context.Context, assignment harnessworker.Assignment, verifiedToken *os.File) (harnessworker.ProcessSpec, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prepared {
		return harnessworker.ProcessSpec{}, errors.New("Hermes adapter is already prepared")
	}
	if err := ctx.Err(); err != nil {
		return harnessworker.ProcessSpec{}, err
	}
	if err := assignment.ValidateAt(a.now().UTC()); err != nil {
		return harnessworker.ProcessSpec{}, fmt.Errorf("Hermes assignment is invalid: %w", err)
	}
	token, err := readVerifiedToken(verifiedToken, assignment.Scope.ListenerTokenFingerprint)
	if err != nil {
		return harnessworker.ProcessSpec{}, err
	}
	abort := make(chan error, 1)
	stdin, err := scopeInput(assignment.Scope)
	if err != nil {
		zero(token)
		return harnessworker.ProcessSpec{}, err
	}
	childToken, err := listenerTokenPipe(token)
	if err != nil {
		zero(token)
		return harnessworker.ProcessSpec{}, err
	}
	delivery := newOutputDelivery(ctx, a.output, abort)
	normal := &normalizer{scope: assignment.Scope, token: token, delivery: delivery, artifactRoot: a.artifactRoot, abort: abort, artifacts: []Artifact{}}
	a.normal, a.prepared = normal, true
	return harnessworker.ProcessSpec{
		Execution: harnessworker.Execution{
			Argv:               []string{ReviewedExecutable, ReviewedArgv[0], ReviewedArgv[1]},
			WorkingDirectory:   "/workspace",
			TimeoutSeconds:     harnessworker.DefaultRunSeconds,
			CancelGraceSeconds: harnessworker.DefaultCancelSeconds,
		},
		Environment: []string{"BLAZN_LISTENER_TOKEN_FD=3", "BLAZN_PROXY_URL=" + a.proxyURL},
		Stdin:       stdin,
		Stdout:      normal,
		ExtraFiles:  []*os.File{childToken},
		Abort:       abort,
	}, nil
}

// Finalize seals parsing and prevents a cancelled execution from being
// reclassified as success by an adapter-authored terminal record.
func (a *Adapter) Finalize(ctx context.Context, process harnessworker.ProcessResult) error {
	a.mu.Lock()
	if !a.prepared || a.normal == nil {
		a.mu.Unlock()
		return errors.New("Hermes adapter was not prepared")
	}
	normal := a.normal
	a.mu.Unlock()
	if err := normal.finalize(ctx); err != nil {
		return err
	}
	if process.Canceled || process.TimedOut || ctx.Err() != nil {
		if normal.status() == "succeeded" {
			return errors.New("Hermes reported success after cancellation")
		}
		if !process.TreeKilled {
			return errors.New("Hermes cancellation did not terminate the process tree")
		}
	}
	if process.Exited && process.ExitCode == 0 && normal.status() != "succeeded" {
		return errors.New("Hermes exited successfully without a successful terminal record")
	}
	return nil
}

func listenerTokenPipe(token []byte) (*os.File, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errors.New("create Hermes listener credential pipe")
	}
	if err := writeAll(writer, token); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, errors.New("write Hermes listener credential pipe")
	}
	if err := writer.Close(); err != nil {
		_ = reader.Close()
		return nil, errors.New("seal Hermes listener credential pipe")
	}
	return reader, nil
}

type Artifact struct {
	Name          string `json:"name"`
	Role          string `json:"role"`
	Kind          string `json:"kind"`
	MediaType     string `json:"mediaType"`
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	ContentDigest string `json:"contentDigest"`
}

func (a *Adapter) Artifacts() []Artifact {
	a.mu.Lock()
	normal := a.normal
	a.mu.Unlock()
	if normal == nil {
		return nil
	}
	normal.mu.Lock()
	defer normal.mu.Unlock()
	return append([]Artifact(nil), normal.artifacts...)
}

// ReviewedArtifacts returns the adapter-attested terminal artifact evidence in
// the worker's shared shape. The runtime compares this defensive copy with its
// independent, post-process rooted collection before reporting success.
func (a *Adapter) ReviewedArtifacts() []harnessworker.ArtifactResult {
	a.mu.Lock()
	normal := a.normal
	a.mu.Unlock()
	if normal == nil {
		return nil
	}
	normal.mu.Lock()
	defer normal.mu.Unlock()
	results := make([]harnessworker.ArtifactResult, 0, len(normal.reviewedArtifacts))
	for _, artifact := range normal.reviewedArtifacts {
		results = append(results, harnessworker.ArtifactResult{Name: artifact.Name, Role: artifact.Role, Kind: artifact.Kind, MediaType: artifact.MediaType, Size: artifact.Size, ContentDigest: artifact.ContentDigest})
	}
	return results
}

// OutputReusable reports whether the adapter's delivery goroutine has
// definitively returned. A false value means that goroutine remains the sole
// owner of the output writer and the caller must suppress its terminal record.
func (a *Adapter) OutputReusable() bool {
	a.mu.Lock()
	normal := a.normal
	a.mu.Unlock()
	return normal == nil || normal.delivery.reusable()
}

type record struct {
	SchemaVersion string                    `json:"schemaVersion"`
	Sequence      int                       `json:"sequence"`
	Type          string                    `json:"type"`
	Payload       map[string]any            `json:"payload"`
	Extensions    map[string]map[string]any `json:"extensions"`
}

type normalizer struct {
	mu                sync.Mutex
	scope             harnessworker.WorkloadScope
	token             []byte
	delivery          *outputDelivery
	artifactRoot      string
	abort             chan<- error
	pending           []byte
	records           int
	artifacts         []Artifact
	reviewedArtifacts []Artifact
	terminalStatus    string
	failed            error
}

type outputDelivery struct {
	ctx    context.Context
	output io.Writer
	abort  chan<- error
	queue  chan []byte
	done   chan struct{}
	once   sync.Once

	mu  sync.Mutex
	err error
}

func newOutputDelivery(ctx context.Context, output io.Writer, abort chan<- error) *outputDelivery {
	delivery := &outputDelivery{ctx: ctx, output: output, abort: abort, queue: make(chan []byte, 8), done: make(chan struct{})}
	go delivery.run()
	return delivery
}

func (d *outputDelivery) run() {
	defer close(d.done)
	for value := range d.queue {
		if err := writeAll(d.output, value); err != nil {
			d.mu.Lock()
			d.err = errors.New("write normalized Hermes output")
			err := d.err
			d.mu.Unlock()
			select {
			case d.abort <- err:
			default:
			}
			return
		}
	}
}

func (d *outputDelivery) send(value []byte) error {
	select {
	case <-d.done:
		return d.result()
	default:
	}
	select {
	case d.queue <- value:
		return nil
	case <-d.done:
		return d.result()
	case <-d.ctx.Done():
		return errors.New("write normalized Hermes output cancelled")
	}
}

func (d *outputDelivery) finish(ctx context.Context) error {
	d.once.Do(func() { close(d.queue) })
	select {
	case <-d.done:
		return d.result()
	case <-ctx.Done():
		return errors.New("flush normalized Hermes output cancelled")
	}
}

func (d *outputDelivery) result() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func (d *outputDelivery) reusable() bool {
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

func writeAll(output io.Writer, value []byte) error {
	for len(value) != 0 {
		written, err := output.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func (n *normalizer) Write(value []byte) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failed != nil {
		return 0, n.failed
	}
	n.pending = append(n.pending, value...)
	if len(n.pending) > maxRecordBytes && !bytes.ContainsRune(n.pending, '\n') {
		return 0, n.fail(errors.New("Hermes output record exceeds 128 KiB"))
	}
	for {
		newline := bytes.IndexByte(n.pending, '\n')
		if newline < 0 {
			break
		}
		line := append([]byte(nil), n.pending[:newline]...)
		n.pending = n.pending[newline+1:]
		if err := n.consume(line); err != nil {
			return 0, n.fail(err)
		}
	}
	return len(value), nil
}

func (n *normalizer) consume(line []byte) error {
	if n.terminalStatus != "" {
		return errors.New("Hermes emitted a record after its terminal result")
	}
	n.records++
	if n.records > maxRecords || len(line) == 0 || len(line) > maxRecordBytes || bytes.Contains(line, n.token) {
		return errors.New("Hermes output record is invalid or contains listener credential material")
	}
	var item record
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if decoder.Decode(&item) != nil || decoder.Decode(&struct{}{}) != io.EOF || item.SchemaVersion != RecordSchema || item.Sequence != n.records || item.Payload == nil || item.Extensions == nil || containsSecret(item, n.token) {
		return errors.New("Hermes output record is invalid or contains forbidden credential material")
	}
	if err := validateRecord(item, n.scope); err != nil {
		return err
	}
	if item.Type == "artifact.created" || item.Type == "patch.created" {
		artifact, err := validateArtifact(item.Payload, n.artifactRoot, n.token)
		if err != nil {
			return err
		}
		if err := n.addArtifact(item.Type, artifact); err != nil {
			return err
		}
	}
	if item.Type == "result.reported" {
		status := text(item.Payload["status"])
		if status == "succeeded" {
			if err := n.requireSuccessfulArtifacts(); err != nil {
				return err
			}
		}
		n.terminalStatus = status
	}
	canonical, _ := json.Marshal(item)
	canonical = append(canonical, '\n')
	if err := n.delivery.send(canonical); err != nil {
		return err
	}
	return nil
}

func (n *normalizer) addArtifact(eventType string, artifact Artifact) error {
	expectedPath := ""
	switch {
	case eventType == "patch.created" && artifact.Name == "patch" && artifact.Role == "patch":
		expectedPath = "/workspace/artifacts/patch.diff"
	case eventType == "artifact.created" && artifact.Name == "summary" && artifact.Role == "summary":
		expectedPath = "/workspace/artifacts/summary.md"
	default:
		return errors.New("Hermes artifact event does not match the required output contract")
	}
	if artifact.Path != expectedPath {
		return errors.New("Hermes artifact path does not match the required output contract")
	}
	for _, existing := range n.artifacts {
		if existing.Name == artifact.Name || existing.Role == artifact.Role || existing.Path == artifact.Path {
			return errors.New("Hermes emitted a duplicate artifact event")
		}
	}
	if artifact.Role == "patch" && len(n.artifacts) != 0 || artifact.Role == "summary" && (len(n.artifacts) != 1 || n.artifacts[0].Role != "patch") {
		return errors.New("Hermes artifact events are out of required order")
	}
	n.artifacts = append(n.artifacts, artifact)
	return nil
}

func (n *normalizer) requireSuccessfulArtifacts() error {
	if len(n.artifacts) != 2 {
		return errors.New("Hermes successful result is missing required artifact events")
	}
	found := map[string]bool{}
	for _, artifact := range n.artifacts {
		found[artifact.Role] = true
	}
	if !found["patch"] || !found["summary"] {
		return errors.New("Hermes successful result is missing required artifact events")
	}
	return nil
}

func (n *normalizer) fail(err error) error {
	if n.failed == nil {
		n.failed = err
		select {
		case n.abort <- err:
		default:
		}
	}
	return n.failed
}

func (n *normalizer) finalize(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	defer zero(n.token)
	outputErr := n.delivery.finish(ctx)
	artifactErr := n.reviewFixedArtifacts()
	if artifactErr != nil {
		return artifactErr
	}
	if n.failed != nil {
		return n.failed
	}
	if len(n.pending) != 0 {
		return errors.New("Hermes output ended with an incomplete JSONL record")
	}
	if outputErr != nil {
		return outputErr
	}
	if n.terminalStatus == "succeeded" {
		if err := n.requireSuccessfulArtifacts(); err != nil {
			return err
		}
	}
	return nil
}

func (n *normalizer) reviewFixedArtifacts() error {
	n.reviewedArtifacts = nil
	reviewed := make([]Artifact, 0, len(n.artifacts))
	for _, fixed := range []Artifact{
		{Name: "patch", Role: "patch", Kind: "agent.patch", MediaType: "text/x-diff", Path: "/workspace/artifacts/patch.diff"},
		{Name: "summary", Role: "summary", Kind: "agent.summary", MediaType: "text/markdown", Path: "/workspace/artifacts/summary.md"},
	} {
		fileSystem, err := sandboxio.OpenRootFileSystem(n.artifactRoot)
		if err != nil {
			return errors.New("open Hermes artifact root")
		}
		contents, readErr := sandboxio.ReadArtifact(fileSystem, fixed.Path, maxArtifactBytes)
		_ = fileSystem.Close()
		if sandboxio.IsProtocolError(readErr, "artifact_not_found") {
			continue
		}
		if readErr != nil {
			return errors.New("Hermes fixed artifact is unavailable or unsafe")
		}
		containsToken := bytes.Contains(contents.Body, n.token)
		zero(contents.Body)
		if containsToken {
			return errors.New("Hermes fixed artifact contains forbidden credential material")
		}
		for _, recorded := range n.artifacts {
			if recorded.Path != fixed.Path {
				continue
			}
			if recorded.Name != fixed.Name || recorded.Role != fixed.Role || recorded.Kind != fixed.Kind || recorded.MediaType != fixed.MediaType || recorded.Size != contents.Size || subtle.ConstantTimeCompare([]byte(recorded.ContentDigest), []byte(contents.SHA256)) != 1 {
				return errors.New("Hermes artifact changed after its creation event")
			}
			reviewed = append(reviewed, recorded)
		}
	}
	if len(reviewed) != len(n.artifacts) {
		return errors.New("Hermes recorded artifact is missing after process exit")
	}
	n.reviewedArtifacts = reviewed
	return nil
}

func artifactPayload(artifact Artifact) map[string]any {
	return map[string]any{"name": artifact.Name, "role": artifact.Role, "kind": artifact.Kind, "mediaType": artifact.MediaType, "path": artifact.Path, "contentDigest": artifact.ContentDigest}
}

func (n *normalizer) status() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.terminalStatus
}

func scopeInput(scope harnessworker.WorkloadScope) (io.Reader, error) {
	encoded, err := json.Marshal(map[string]any{"schemaVersion": RecordSchema, "sequence": 0, "type": "scope", "payload": map[string]any{"scope": scope}, "extensions": map[string]any{}})
	if err != nil {
		return nil, errors.New("encode Hermes scope")
	}
	return bytes.NewReader(append(encoded, '\n')), nil
}

func readVerifiedToken(file *os.File, fingerprint string) ([]byte, error) {
	if file == nil {
		return nil, errors.New("Hermes verified listener credential is required")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.New("read Hermes verified listener credential")
	}
	token, err := io.ReadAll(io.LimitReader(file, maxTokenBytes+1))
	if err != nil || len(token) < 16 || len(token) > maxTokenBytes || bytes.ContainsAny(token, "\x00\r\n") || len(bytes.TrimSpace(token)) != len(token) {
		zero(token)
		return nil, errors.New("Hermes verified listener credential is invalid")
	}
	digest := sha256.Sum256(token)
	expected, err := decodeDigest(fingerprint)
	if err != nil || subtle.ConstantTimeCompare(digest[:], expected) != 1 {
		zero(token)
		return nil, errors.New("Hermes listener token fingerprint does not match workload scope")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		zero(token)
		return nil, errors.New("rewind Hermes verified listener credential")
	}
	return token, nil
}

func validateRecord(item record, scope harnessworker.WorkloadScope) error {
	switch item.Type {
	case "harness.started", "harness.waiting", "harness.resumed", "harness.stopping", "harness.exited", "message.assistant", "tool.requested", "tool.started", "tool.completed", "tool.failed", "approval.requested", "progress.updated", "cancellation.acknowledged", "artifact.created", "patch.created":
	case "result.reported":
		status := text(item.Payload["status"])
		if status != "succeeded" && status != "failed" && status != "cancelled" {
			return errors.New("Hermes terminal status is invalid")
		}
	case "model.requested", "model.usage":
		if text(item.Payload["routeId"]) != scope.RouteID || integer(item.Payload["routeVersion"]) != scope.RouteVersion || text(item.Payload["protocol"]) != string(scope.Protocol) {
			return errors.New("Hermes model record does not match the frozen route binding")
		}
	default:
		return errors.New("Hermes output record type is unsupported")
	}
	for namespace, values := range item.Extensions {
		if !strings.HasPrefix(namespace, "hermes.") || len(values) > 16 {
			return errors.New("Hermes output extension is outside its namespace")
		}
		for _, value := range values {
			switch scalar := value.(type) {
			case string:
				if len(scalar) > 512 {
					return errors.New("Hermes output extension string exceeds 512 bytes")
				}
			case json.Number:
				if len(scalar.String()) > 64 {
					return errors.New("Hermes output extension number is invalid")
				}
			case bool, nil:
			default:
				return errors.New("Hermes output extension values must be bounded scalars")
			}
		}
	}
	return nil
}

func validateArtifact(payload map[string]any, artifactRoot string, token []byte) (Artifact, error) {
	artifact := Artifact{Name: text(payload["name"]), Role: text(payload["role"]), Kind: text(payload["kind"]), MediaType: text(payload["mediaType"]), Path: text(payload["path"]), ContentDigest: text(payload["contentDigest"])}
	if artifact.Name == "" {
		return Artifact{}, errors.New("Hermes artifact path is invalid")
	}
	switch artifact.Role {
	case "patch":
		if artifact.Kind != "agent.patch" || artifact.MediaType != "text/x-diff" {
			return Artifact{}, errors.New("Hermes patch artifact metadata is invalid")
		}
	case "summary":
		if artifact.Kind != "agent.summary" || artifact.MediaType != "text/markdown" {
			return Artifact{}, errors.New("Hermes summary artifact metadata is invalid")
		}
	case "output":
		if artifact.Kind != "agent.output" || artifact.MediaType != "text/markdown" && artifact.MediaType != "application/octet-stream" {
			return Artifact{}, errors.New("Hermes output artifact metadata is invalid")
		}
	default:
		return Artifact{}, errors.New("Hermes artifact role is invalid")
	}
	fileSystem, err := sandboxio.OpenRootFileSystem(artifactRoot)
	if err != nil {
		return Artifact{}, errors.New("open Hermes artifact root")
	}
	defer fileSystem.Close()
	contents, err := sandboxio.ReadArtifact(fileSystem, artifact.Path, maxArtifactBytes)
	if err != nil {
		return Artifact{}, errors.New("Hermes artifact is unavailable or unsafe")
	}
	defer zero(contents.Body)
	if bytes.Contains(contents.Body, token) {
		return Artifact{}, errors.New("Hermes artifact contains forbidden credential material")
	}
	if subtle.ConstantTimeCompare([]byte(contents.SHA256), []byte(artifact.ContentDigest)) != 1 {
		return Artifact{}, errors.New("Hermes artifact digest does not match")
	}
	artifact.Size = contents.Size
	return artifact, nil
}

func validateProxyURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return "", errors.New("Hermes proxy URL must be an HTTP loopback origin")
	}
	host := parsed.Hostname()
	parsedIP := net.ParseIP(host)
	if parsed.Port() == "" || parsedIP == nil || !parsedIP.IsLoopback() {
		return "", errors.New("Hermes proxy URL must be an HTTP loopback origin")
	}
	return parsed.String(), nil
}

func decodeDigest(value string) ([]byte, error) {
	if !strings.HasPrefix(value, "sha256:") {
		return nil, errors.New("invalid digest")
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New("invalid digest")
	}
	return decoded, nil
}

var credentialValue = regexp.MustCompile(`(?i)(?:bearer\s+|sk-[a-z0-9_-]{12}|github_pat_|ghp_[a-z0-9]{12}|xox[baprs]-|akia[0-9a-z]{12})`)

func containsSecret(value any, token []byte) bool {
	switch current := value.(type) {
	case record:
		return containsSecret(current.Payload, token) || containsSecret(current.Extensions, token)
	case map[string]any:
		for key, nested := range current {
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(key))
			switch normalized {
			case "token", "listenertoken", "password", "secret", "credential", "authorization", "apikey", "privatekey", "kubeconfig", "nodecredential":
				return true
			}
			if containsSecret(nested, token) {
				return true
			}
		}
	case map[string]map[string]any:
		for _, nested := range current {
			if containsSecret(nested, token) {
				return true
			}
		}
	case []any:
		for _, nested := range current {
			if containsSecret(nested, token) {
				return true
			}
		}
	case string:
		return bytes.Contains([]byte(current), token) || credentialValue.MatchString(current)
	}
	return false
}

func text(value any) string {
	text, _ := value.(string)
	return text
}

func integer(value any) int64 {
	switch number := value.(type) {
	case json.Number:
		result, _ := number.Int64()
		return result
	case float64:
		return int64(number)
	case int64:
		return number
	case int:
		return int64(number)
	default:
		return 0
	}
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ harnessworker.Adapter = (*Adapter)(nil)
var _ io.Writer = (*normalizer)(nil)
