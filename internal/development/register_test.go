package development

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

const (
	registerWorkspaceID = "10000000-0000-4000-8000-000000000001"
	registerProjectID   = "20000000-0000-4000-8000-000000000001"
)

type registerSessions struct {
	origin string
	forced int
}

func (s *registerSessions) Origin() string { return s.origin }
func (s *registerSessions) Session(_ context.Context, force bool) (workspacepkg.Session, error) {
	if force {
		s.forced++
	}
	return workspacepkg.Session{AccessToken: "access-token", UserID: "user-1"}, nil
}

type registerContexts struct{}

func (registerContexts) Load(string, string) (workspacepkg.Selection, error) {
	return workspacepkg.Selection{WorkspaceID: registerWorkspaceID, ProjectID: registerProjectID}, nil
}
func (registerContexts) Save(workspacepkg.Selection) error { return nil }

func TestRegisterUsesSelectedContextAndStableIdempotencyThenBuilds(t *testing.T) {
	data, err := os.ReadFile("../../packages/contracts/testdata/development/project-good.json")
	if err != nil {
		t.Fatal(err)
	}
	validation, manifest := Validate(data)
	if !validation.Valid || manifest == nil {
		t.Fatalf("fixture is invalid: %#v", validation)
	}
	puts, builds := 0, 0
	operations := []string{}
	var registrationBodies [][]byte
	projectJSON := []byte(`{"workspaceId":"` + registerWorkspaceID + `","projectId":"` + registerProjectID + `","version":1,"manifest":` + string(data) + `,"manifestDigest":"` + *validation.ManifestDigest + `","createdBy":"10000000-0000-4000-8000-000000000002","createdAt":"2026-08-24T00:00:00Z","updatedAt":"2026-08-24T00:00:00Z"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("Accept") != "application/json" {
			t.Fatalf("missing authentication headers: %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/workspaces/"+registerWorkspaceID+"/projects/"+registerProjectID+"/development-project":
			puts++
			operations = append(operations, "register")
			if r.Header.Get("Idempotency-Key") != "register-request-1" {
				t.Fatalf("idempotency key=%q", r.Header.Get("Idempotency-Key"))
			}
			var body struct {
				ExpectedVersion int      `json:"expectedVersion"`
				Manifest        Manifest `json:"manifest"`
			}
			payload, readErr := io.ReadAll(r.Body)
			registrationBodies = append(registrationBodies, payload)
			if err := json.Unmarshal(payload, &body); err != nil || readErr != nil || body.ExpectedVersion != 0 || body.Manifest.ProjectID != registerProjectID || body.Manifest.Build.Context != "." || body.Manifest.Policy.BuilderProfile == "" {
				t.Fatalf("registration body=%#v err=%v", body, err)
			}
			_, _ = w.Write(append(append([]byte(`{"project":`), projectJSON...), '}'))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/workspaces/"+registerWorkspaceID+"/projects/"+registerProjectID+"/development-builds":
			builds++
			operations = append(operations, "build")
			var body struct {
				Commit string `json:"commit"`
			}
			if r.Header.Get("Idempotency-Key") != "build-request-1" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Commit != "1111111111111111111111111111111111111111" {
				t.Fatalf("invalid immediate build request: key=%q body=%#v", r.Header.Get("Idempotency-Key"), body)
			}
			_, _ = w.Write([]byte(`{"build":{"schemaVersion":"blazn.dev/build-status/v1alpha1","id":"30000000-0000-4000-8000-000000000001","status":"queued","version":1,"receiptDigest":null}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	sessions := &registerSessions{origin: server.URL}
	service := NewService(sessions, registerContexts{}, server.Client())
	var persistedOutputs [][]byte
	for range 2 {
		project, err := service.Register(context.Background(), *manifest, 0, "register-request-1")
		if err != nil {
			t.Fatal(err)
		}
		if projectID, version, _ := project.Summary(); projectID != registerProjectID || version != 1 {
			t.Fatalf("project=%#v", project)
		}
		persisted, marshalErr := json.Marshal(project)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		persistedOutputs = append(persistedOutputs, persisted)
	}
	if _, err := service.Build(context.Background(), "1111111111111111111111111111111111111111", "build-request-1"); err != nil {
		t.Fatal(err)
	}
	var expectedProject, firstProject, secondProject any
	if err := json.Unmarshal(projectJSON, &expectedProject); err != nil {
		t.Fatal(err)
	}
	if len(persistedOutputs) == 2 {
		if err := json.Unmarshal(persistedOutputs[0], &firstProject); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(persistedOutputs[1], &secondProject); err != nil {
			t.Fatal(err)
		}
	}
	if puts != 2 || builds != 1 || len(registrationBodies) != 2 || string(registrationBodies[0]) != string(registrationBodies[1]) || len(persistedOutputs) != 2 || !reflect.DeepEqual(firstProject, expectedProject) || !reflect.DeepEqual(secondProject, expectedProject) {
		t.Fatalf("puts=%d builds=%d persisted=%d", puts, builds, len(persistedOutputs))
	}
	if want := []string{"register", "register", "build"}; len(operations) != len(want) || operations[0] != want[0] || operations[1] != want[1] || operations[2] != want[2] {
		t.Fatalf("operations=%v", operations)
	}
}

func TestDecodeProjectRejectsMalformedPersistedOutput(t *testing.T) {
	data, err := os.ReadFile("../../packages/contracts/testdata/development/project-good.json")
	if err != nil {
		t.Fatal(err)
	}
	validation, _ := Validate(data)
	valid := `{"workspaceId":"` + registerWorkspaceID + `","projectId":"` + registerProjectID + `","version":1,"manifest":` + string(data) + `,"manifestDigest":"` + *validation.ManifestDigest + `","createdBy":"10000000-0000-4000-8000-000000000002","createdAt":"2026-08-24T00:00:00Z","updatedAt":"2026-08-24T00:00:00Z"}`
	if _, err := DecodeProject([]byte(valid)); err != nil {
		t.Fatalf("valid persisted output rejected: %v", err)
	}
	for name, candidate := range map[string]string{
		"unknown field":                     valid[:len(valid)-1] + `,"accessToken":"secret-value"}`,
		"bad creator":                       strings.Replace(valid, "10000000-0000-4000-8000-000000000002", "not-a-user", 1),
		"bad timestamp":                     strings.Replace(valid, "2026-08-24T00:00:00Z", "yesterday", 1),
		"wrong workspace":                   strings.Replace(valid, registerWorkspaceID, "not-a-workspace", 1),
		"shadowed credential-bearing field": strings.Replace(valid, `"manifestDigest":"`, `"manifestDigest":"github_pat_12345678901234567890","manifestDigest":"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			project, err := DecodeProject([]byte(candidate))
			if err == nil {
				t.Fatal("malformed persisted output accepted")
			}
			if strings.Contains(string(project.raw), "github_pat_") {
				t.Fatal("rejected Project retained unsafe raw output")
			}
			if strings.Contains(err.Error(), "github_pat_") {
				t.Fatal("rejected Project exposed unsafe response content in its error")
			}
		})
	}
}

func TestRegisterRejectsManifestForAnotherSelectedProjectBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("mismatched project reached the API")
	}))
	defer server.Close()
	manifest := Manifest{ProjectID: "20000000-0000-4000-8000-000000000099"}
	service := NewService(&registerSessions{origin: server.URL}, registerContexts{}, server.Client())
	if _, err := service.Register(context.Background(), manifest, 0, "register-request-1"); err == nil {
		t.Fatal("mismatched Project identity accepted")
	}
}
