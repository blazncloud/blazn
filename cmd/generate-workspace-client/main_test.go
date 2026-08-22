package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkedInWorkspaceContract(t *testing.T) (map[string]any, []byte) {
	t.Helper()
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(root, "packages", "contracts", "workspaces.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	return document, encoded
}

func TestWorkspaceContractAndPinnedDigestMatchGenerator(t *testing.T) {
	document, encoded := checkedInWorkspaceContract(t)
	if err := validate(document, string(workspaceTemplate)); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	if got := hex.EncodeToString(digest[:]); got != supportedWorkspaceContractSHA256 {
		t.Fatalf("contract digest=%s pinned=%s", got, supportedWorkspaceContractSHA256)
	}
}

func TestWorkspaceValidatorRejectsHeaderAndPathDrift(t *testing.T) {
	document, encoded := checkedInWorkspaceContract(t)
	var changed map[string]any
	if err := json.Unmarshal(encoded, &changed); err != nil {
		t.Fatal(err)
	}
	parameters := at(changed, "paths", "/v1/workspaces", "post", "parameters").([]any)
	parameters[0].(map[string]any)["$ref"] = "#/components/parameters/Cursor"
	if err := validate(changed, string(workspaceTemplate)); err == nil || !strings.Contains(err.Error(), "parameter") {
		t.Fatalf("header drift error=%v", err)
	}

	if err := json.Unmarshal(encoded, &changed); err != nil {
		t.Fatal(err)
	}
	delete(changed["paths"].(map[string]any), "/v1/workspace-invitations/accept")
	if err := validate(changed, string(workspaceTemplate)); err == nil || !strings.Contains(err.Error(), "paths changed") {
		t.Fatalf("path drift error=%v", err)
	}
}

func TestWorkspaceTemplateNeverPlacesInvitationTokenInURL(t *testing.T) {
	template := string(workspaceTemplate)
	if !strings.Contains(template, "json.Marshal(input)") || !strings.Contains(template, "request.InviteToken") {
		t.Fatal("workspace invitation acceptance is not body-backed")
	}
	for _, unsafe := range []string{"PathEscape(request.InviteToken)", `query.Set("inviteToken"`, `RawQuery = request.InviteToken`} {
		if strings.Contains(template, unsafe) {
			t.Fatalf("workspace template contains unsafe token routing: %s", unsafe)
		}
	}
}
