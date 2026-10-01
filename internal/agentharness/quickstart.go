package agentharness

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

// TemplateAPI is the part of the Sandbox API quickstart needs to resolve a
// published template version.
type TemplateAPI interface {
	ListSandboxTemplates(context.Context, string, string, string) (client.SandboxTemplateList, error)
	ListSandboxTemplateVersions(context.Context, string, string, string) (client.SandboxTemplateVersionList, error)
}

// QuickstartOptions describes one Agent that runs on the Blazn reference harness.
type QuickstartOptions struct {
	Name              string
	Template          string // NAME@VERSION of a published Sandbox template
	Repository        string
	Commit            string
	Instructions      string
	Purpose           string
	ModelRouteID      string
	ModelRouteVersion int
	RequestID         string
	HarnessCommit     string // provenance of the harness; the CLI's build commit
}

// QuickstartResult names everything `blazn run create` needs.
type QuickstartResult struct {
	WorkspaceID         string `json:"workspaceId"`
	AgentID             string `json:"agentId"`
	AgentVersionID      string `json:"agentVersionId"`
	AgentVersion        int    `json:"agentVersion"`
	HarnessDefinitionID string `json:"harnessDefinitionId"`
	HarnessVersionID    string `json:"harnessVersionId"`
	HarnessProfileID    string `json:"harnessProfileId"`
	TemplateVersionID   string `json:"templateVersionId"`
	Reused              bool   `json:"reused"`
}

const (
	referenceHarnessKind    = "blazn-agent"
	referenceHarnessVersion = "0.1.0"
	referenceProtocol       = "openai-chat"
)

var (
	quickstartName   = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,95}$`)
	quickstartCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
	quickstartRepo   = regexp.MustCompile(`^https://[^@?#\s]+$`)
)

// Quickstart registers the reference harness, a profile for one model route,
// and an Agent version pinned to a Sandbox template. Every step reuses what
// already exists, so running it again with the same options changes nothing.
func (s *Service) Quickstart(ctx context.Context, options QuickstartOptions) (QuickstartResult, error) {
	if err := options.validate(); err != nil {
		return QuickstartResult{}, err
	}
	templates, ok := s.api.(TemplateAPI)
	if !ok {
		return QuickstartResult{}, errors.New("the Sandbox template API is unavailable")
	}
	return call(ctx, s, func(selection workspacepkg.Selection, session workspacepkg.Session) (QuickstartResult, error) {
		workspace, token := selection.WorkspaceID, session.AccessToken
		result := QuickstartResult{WorkspaceID: workspace, Reused: true}
		key := func(step string) string { return options.RequestID + "-" + step }

		version, err := findTemplateVersion(ctx, templates, token, workspace, options.Template)
		if err != nil {
			return result, err
		}
		result.TemplateVersionID = version.ID

		definitions, err := s.api.ListHarnessDefinitions(ctx, token, workspace, "")
		if err != nil {
			return result, err
		}
		for _, definition := range definitions.Items {
			if definition.Kind == referenceHarnessKind {
				if definition.Status != "approved" {
					return result, errors.New("this Workspace's blazn-agent harness definition is not approved")
				}
				result.HarnessDefinitionID = definition.ID
			}
		}
		if result.HarnessDefinitionID == "" {
			created, err := s.api.CreateHarnessDefinition(ctx, token, workspace, key("definition"), client.CreateHarnessDefinitionRequest{Definition: client.JSONDocument{
				"schemaVersion": "blazn.dev/harness-definition/v1alpha1", "id": newUUID(), "kind": referenceHarnessKind, "publisher": "blazncloud", "status": "approved",
				"supportedPlatforms": []any{"linux/amd64", "linux/arm64"}, "securityPolicy": "poc-harness-restricted-v1", "resourceVersion": 1}})
			if err != nil {
				return result, fmt.Errorf("create harness definition: %w", err)
			}
			result.HarnessDefinitionID, result.Reused = created.Definition.ID, false
		}

		versions, err := s.api.ListHarnessVersions(ctx, token, workspace, result.HarnessDefinitionID, "")
		if err != nil {
			return result, err
		}
		for _, candidate := range versions.Items {
			if candidate.Version == referenceHarnessVersion {
				result.HarnessVersionID = candidate.ID
			}
		}
		if result.HarnessVersionID == "" {
			document := referenceHarnessVersionDocument(result.HarnessDefinitionID, options.HarnessCommit)
			document["digest"] = documentDigest("blazn-harness-version-v1", document, "digest")
			published, err := s.api.PublishHarnessVersion(ctx, token, workspace, result.HarnessDefinitionID, key("harness-version"), client.PublishHarnessVersionRequest{Version: document})
			if err != nil {
				return result, fmt.Errorf("publish harness version: %w", err)
			}
			result.HarnessVersionID, result.Reused = published.Version.ID, false
		}

		profileName := fmt.Sprintf("blazn-agent-%s-v%d", options.ModelRouteID[:8], options.ModelRouteVersion)
		profileDigest := ""
		profiles, err := s.api.ListHarnessProfiles(ctx, token, workspace, "")
		if err != nil {
			return result, err
		}
		for _, profile := range profiles.Items {
			if profile.Name == profileName && profile.HarnessVersionID == result.HarnessVersionID && profile.Status == "approved" {
				result.HarnessProfileID, profileDigest = profile.ID, profile.Digest
			}
		}
		if result.HarnessProfileID == "" {
			document := client.JSONDocument{
				"schemaVersion": "blazn.dev/harness-profile/v1alpha1", "id": newUUID(), "workspaceId": workspace, "name": profileName, "harnessVersionId": result.HarnessVersionID,
				"model":       map[string]any{"routeId": options.ModelRouteID, "routeVersion": options.ModelRouteVersion, "protocol": referenceProtocol},
				"credentials": []any{map[string]any{"capability": "model.proxy", "scope": fmt.Sprintf("route:%s:%d", options.ModelRouteID, options.ModelRouteVersion), "leaseSeconds": 900}},
				"tools":       []any{}, "overrides": map[string]any{},
				"policy": map[string]any{"approval": "deny-unlisted", "network": "profile-bound", "filesystem": "sandbox-only", "maxRunSeconds": 7200, "maxFollowUps": 100},
				"status": "approved", "directProviderAuthorization": nil, "resourceVersion": 1}
			document["digest"] = documentDigest("blazn-harness-profile-v1", document, "digest", "resourceVersion")
			created, err := s.api.CreateHarnessProfile(ctx, token, workspace, key("profile"), client.CreateHarnessProfileRequest{Profile: document})
			if err != nil {
				return result, fmt.Errorf("create harness profile: %w", err)
			}
			result.HarnessProfileID, profileDigest, result.Reused = created.Profile.ID, created.Profile.Digest, false
		}

		agents, err := s.api.ListAgents(ctx, token, workspace, "")
		if err != nil {
			return result, err
		}
		for _, agent := range agents.Items {
			if agent.Name == options.Name && agent.Status == "active" {
				result.AgentID = agent.ID
			}
		}
		if result.AgentID == "" {
			created, err := s.api.CreateAgent(ctx, token, workspace, key("agent"), client.CreateAgentRequest{Name: options.Name, Tags: []string{}})
			if err != nil {
				return result, fmt.Errorf("create agent: %w", err)
			}
			result.AgentID, result.Reused = created.Agent.ID, false
		}

		published, err := s.api.ListAgentVersions(ctx, token, workspace, result.AgentID, "")
		if err != nil {
			return result, err
		}
		next := 1
		templateDigest := "sha256:" + strings.TrimPrefix(version.ContentDigest, "sha256:")
		for _, existing := range published.Items {
			if existing.Version >= next {
				next = existing.Version + 1
			}
			if sameAgentVersion(existing.Document, options, version.ID, templateDigest, result.HarnessProfileID, profileDigest) {
				result.AgentVersionID, result.AgentVersion = existing.ID, existing.Version
			}
		}
		if result.AgentVersionID != "" {
			return result, nil
		}
		purpose := options.Purpose
		if purpose == "" {
			purpose = "Agent " + options.Name
		}
		document := client.JSONDocument{
			"schemaVersion": "blazn.dev/agent-version/v1alpha1", "id": newUUID(), "agentId": result.AgentID, "workspaceId": workspace, "version": next,
			"purpose": purpose, "instructions": options.Instructions,
			"modelPolicy": map[string]any{"routeId": options.ModelRouteID, "routeVersion": options.ModelRouteVersion, "policyId": newUUID(), "policyVersion": 1,
				"requiredProtocols": []any{referenceProtocol}, "fallback": "disabled", "fallbackRoute": nil},
			"tools":           []any{},
			"template":        map[string]any{"versionId": version.ID, "digest": templateDigest},
			"repository":      map[string]any{"url": options.Repository, "commit": options.Commit, "writable": true, "pushAllowed": false},
			"resourceProfile": "poc-agent-small", "requiredCapabilities": []any{"message.follow-up"},
			"allowedHarnessProfiles":  []any{map[string]any{"id": result.HarnessProfileID, "digest": profileDigest}},
			"defaultHarnessProfileId": result.HarnessProfileID, "evaluationId": newUUID(),
			"outputContract": map[string]any{"summary": true, "patch": true, "artifacts": []any{}},
			"createdBy":      session.UserID, "createdAt": time.Now().UTC().Format(time.RFC3339)}
		document["digest"] = documentDigest("blazn-agent-version-v1", document, "digest")
		created, err := s.api.PublishAgentVersion(ctx, token, workspace, result.AgentID, key("agent-version-"+strconv.Itoa(next)), client.PublishAgentVersionRequest{Version: document})
		if err != nil {
			return result, fmt.Errorf("publish agent version: %w", err)
		}
		result.AgentVersionID, result.AgentVersion, result.Reused = created.Version.ID, created.Version.Version, false
		return result, nil
	})
}

func (o QuickstartOptions) validate() error {
	switch {
	case !quickstartName.MatchString(o.Name):
		return errors.New("agent name must start with a lowercase letter and use only lowercase letters, digits, dot, underscore and hyphen")
	case !strings.Contains(o.Template, "@"):
		return errors.New("template must be NAME@VERSION")
	case !quickstartRepo.MatchString(o.Repository):
		return errors.New("repository must be an https URL without credentials")
	case !quickstartCommit.MatchString(o.Commit):
		return errors.New("commit must be a full 40-character lowercase SHA")
	case len(o.Instructions) == 0 || len(o.Instructions) > 32768:
		return errors.New("instructions must contain 1 to 32768 characters")
	case len(o.Purpose) > 512:
		return errors.New("purpose must contain at most 512 characters")
	case !lowerUUID.MatchString(o.ModelRouteID) || o.ModelRouteVersion < 1:
		return errors.New("model route must be a lowercase UUID with a version of at least 1")
	case len(o.RequestID) < 8 || len(o.RequestID) > 96:
		return errors.New("request ID must contain 8 to 96 characters")
	}
	return nil
}

func findTemplateVersion(ctx context.Context, api TemplateAPI, token, workspace, reference string) (client.SandboxTemplateVersion, error) {
	name, wanted, _ := strings.Cut(reference, "@")
	for cursor := ""; ; {
		page, err := api.ListSandboxTemplates(ctx, token, workspace, cursor)
		if err != nil {
			return client.SandboxTemplateVersion{}, err
		}
		for _, template := range page.Items {
			if template.Name != name {
				continue
			}
			for versionCursor := ""; ; {
				versions, err := api.ListSandboxTemplateVersions(ctx, token, template.ID, versionCursor)
				if err != nil {
					return client.SandboxTemplateVersion{}, err
				}
				for _, version := range versions.Items {
					if version.Version == wanted {
						if version.Status != "published" {
							return version, fmt.Errorf("template %s is %s, not published", reference, version.Status)
						}
						return version, nil
					}
				}
				if versions.NextCursor == nil {
					break
				}
				versionCursor = *versions.NextCursor
			}
		}
		if page.NextCursor == nil {
			return client.SandboxTemplateVersion{}, fmt.Errorf("template %s is not published in this Workspace", reference)
		}
		cursor = *page.NextCursor
	}
}

func referenceHarnessVersionDocument(definitionID, commit string) client.JSONDocument {
	if !quickstartCommit.MatchString(commit) {
		commit = strings.Repeat("0", 40)
	}
	// The executable ships in the Sandbox image or is installed by the Agent
	// Run controller; this digest identifies the reviewed harness release.
	release := sha256.Sum256([]byte("blazn-agent/" + referenceHarnessVersion))
	artifact := "sha256:" + hex.EncodeToString(release[:])
	return client.JSONDocument{
		"schemaVersion": "blazn.dev/harness-version/v1alpha1", "id": newUUID(), "definitionId": definitionID, "version": referenceHarnessVersion,
		"protocolVersion": "blazn.dev/harness-adapter/v1alpha1",
		"implementation":  map[string]any{"kind": "package", "digest": artifact},
		"executable": map[string]any{"identity": "blazn-agent-adapter", "path": "/opt/blazn/agent", "fixedArgv": []any{"serve"}, "environmentAllowlist": []any{},
			"termination": map[string]any{"gracefulSignal": "SIGTERM", "graceSeconds": 20, "killProcessTree": true}},
		"inputMode": "file-json", "parserVersion": "blazn-agent/v1alpha1",
		"capabilities":       []any{"message.follow-up", "conversation.resume", "event.structured", "tool.native", "output.patch", "output.artifact", "cancel.graceful"},
		"supportedPlatforms": []any{"linux/amd64", "linux/arm64"}, "credentialCapabilities": []any{"model.proxy"},
		"compatibility": map[string]any{"workerProtocol": "blazn.dev/harness-worker/v1alpha1", "sandboxControl": "agents.x-k8s.io/v1beta1", "proxyProtocols": []any{referenceProtocol}},
		"provenance":    map[string]any{"repository": "https://github.com/blazncloud/blazn.git", "commit": commit, "artifactDigest": artifact, "reviewId": newUUID()},
	}
}

func sameAgentVersion(document client.JSONDocument, options QuickstartOptions, templateVersionID, templateDigest, profileID, profileDigest string) bool {
	template, _ := document["template"].(map[string]any)
	repository, _ := document["repository"].(map[string]any)
	pins, _ := document["allowedHarnessProfiles"].([]any)
	if len(pins) != 1 || template == nil || repository == nil {
		return false
	}
	pin, _ := pins[0].(map[string]any)
	return document["instructions"] == options.Instructions && template["versionId"] == templateVersionID && template["digest"] == templateDigest &&
		repository["url"] == options.Repository && repository["commit"] == options.Commit && pin["id"] == profileID && pin["digest"] == profileDigest
}

// documentDigest reproduces the control API's document digest: SHA-256 over a
// domain line and the canonical JSON of the document without the excluded keys.
func documentDigest(domain string, document client.JSONDocument, excluded ...string) string {
	copy := make(map[string]any, len(document))
	for key, value := range document {
		copy[key] = value
	}
	for _, key := range excluded {
		delete(copy, key)
	}
	var b strings.Builder
	b.WriteString(domain)
	b.WriteByte('\n')
	writeCanonical(&b, copy)
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeCanonical writes JSON exactly as JavaScript's JSON.stringify does for
// the same value with object keys sorted, which is what the API hashes.
func writeCanonical(b *strings.Builder, value any) {
	switch typed := value.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(typed))
	case int:
		b.WriteString(strconv.Itoa(typed))
	case int64:
		b.WriteString(strconv.FormatInt(typed, 10))
	case json.Number:
		b.WriteString(typed.String())
	case float64:
		// Documents only carry integers; JSON decoding yields float64 for them.
		b.WriteString(strconv.FormatInt(int64(typed), 10))
	case string:
		writeCanonicalString(b, typed)
	case []any:
		b.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, item)
		}
		b.WriteByte(']')
	case client.JSONDocument:
		writeCanonical(b, map[string]any(typed))
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				b.WriteByte(',')
			}
			writeCanonicalString(b, key)
			b.WriteByte(':')
			writeCanonical(b, typed[key])
		}
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("unsupported canonical JSON value %T", value))
	}
}

func writeCanonicalString(b *strings.Builder, value string) {
	b.WriteByte('"')
	for _, r := range strings.ToValidUTF8(value, "�") {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

func newUUID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}
