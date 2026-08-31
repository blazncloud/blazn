package agentharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

type API interface {
	CreateAgent(context.Context, string, string, string, client.CreateAgentRequest) (client.AgentEnvelope, error)
	ListAgents(context.Context, string, string, string) (client.AgentList, error)
	GetAgent(context.Context, string, string, string) (client.AgentEnvelope, error)
	PublishAgentVersion(context.Context, string, string, string, string, client.PublishAgentVersionRequest) (client.PublishAgentVersionEnvelope, error)
	ListAgentVersions(context.Context, string, string, string, string) (client.AgentVersionList, error)
	GetAgentVersion(context.Context, string, string, string, string) (client.AgentVersionEnvelope, error)
	CreateHarnessDefinition(context.Context, string, string, string, client.CreateHarnessDefinitionRequest) (client.HarnessDefinitionEnvelope, error)
	ListHarnessDefinitions(context.Context, string, string, string) (client.HarnessDefinitionList, error)
	GetHarnessDefinition(context.Context, string, string, string) (client.HarnessDefinitionEnvelope, error)
	PublishHarnessVersion(context.Context, string, string, string, string, client.PublishHarnessVersionRequest) (client.HarnessVersionEnvelope, error)
	ListHarnessVersions(context.Context, string, string, string, string) (client.HarnessVersionList, error)
	GetHarnessVersion(context.Context, string, string, string, string) (client.HarnessVersionEnvelope, error)
	CreateHarnessProfile(context.Context, string, string, string, client.CreateHarnessProfileRequest) (client.HarnessProfileEnvelope, error)
	ListHarnessProfiles(context.Context, string, string, string) (client.HarnessProfileList, error)
	GetHarnessProfile(context.Context, string, string, string) (client.HarnessProfileEnvelope, error)
	ReviseHarnessProfile(context.Context, string, string, string, string, client.ReviseHarnessProfileRequest) (client.HarnessProfileEnvelope, error)
}
type Service struct {
	api      API
	sessions workspacepkg.SessionProvider
	contexts workspacepkg.ContextStore
}

func NewDefaultService() (*Service, error) {
	s, e := workspacepkg.NewDefaultSessionProvider()
	if e != nil {
		return nil, e
	}
	a, e := client.New(s.Origin(), &http.Client{Timeout: 30 * time.Second})
	if e != nil {
		return nil, e
	}
	c, e := workspacepkg.NewFileContextStore()
	if e != nil {
		return nil, e
	}
	return NewService(a, s, c), nil
}
func NewService(a API, s workspacepkg.SessionProvider, c workspacepkg.ContextStore) *Service {
	return &Service{api: a, sessions: s, contexts: c}
}
func (s *Service) selected(ctx context.Context) (workspacepkg.Selection, workspacepkg.Session, error) {
	session, e := s.sessions.Session(ctx, false)
	if e != nil {
		return workspacepkg.Selection{}, session, e
	}
	sel, e := s.contexts.Load(s.sessions.Origin(), session.UserID)
	return sel, session, e
}
func call[T any](ctx context.Context, s *Service, fn func(workspacepkg.Selection, workspacepkg.Session) (T, error)) (T, error) {
	sel, session, e := s.selected(ctx)
	if e != nil {
		var z T
		return z, e
	}
	out, e := fn(sel, session)
	if client.IsCode(e, "access_expired") {
		session, e = s.sessions.Session(ctx, true)
		if e != nil {
			var z T
			return z, e
		}
		return fn(sel, session)
	}
	return out, e
}

func ReadDocument(path string) (client.JSONDocument, error) {
	data, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	if len(data) > 512*1024 {
		return nil, errors.New("document exceeds 512 KiB")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var d client.JSONDocument
	if e = dec.Decode(&d); e != nil {
		return nil, fmt.Errorf("decode JSON document: %w", e)
	}
	if d == nil {
		return nil, errors.New("document must be a JSON object")
	}
	var trailing any
	if trailingErr := dec.Decode(&trailing); trailingErr != io.EOF {
		return nil, errors.New("document contains trailing JSON")
	}
	return d, nil
}

var schemaOnce sync.Once
var documentSchemas map[string]*jsonschema.Schema
var documentSchemaErr error
var lowerUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func ValidateDocument(kind string, d client.JSONDocument) error {
	schemaOnce.Do(compileDocumentSchemas)
	if documentSchemaErr != nil {
		return fmt.Errorf("compile normative schemas: %w", documentSchemaErr)
	}
	schema, ok := documentSchemas[kind]
	if !ok {
		return fmt.Errorf("unknown document kind %q", kind)
	}
	if err := schema.Validate(map[string]any(d)); err != nil {
		return fmt.Errorf("%s violates the normative schema: %w", kind, err)
	}
	for _, field := range identifierFields(kind) {
		if value, ok := d[field].(string); ok && !lowerUUID.MatchString(value) {
			return fmt.Errorf("%s field %s must be a lowercase UUID", kind, field)
		}
	}
	return nil
}

func identifierFields(kind string) []string {
	switch kind {
	case "agent-version":
		return []string{"id", "agentId", "workspaceId", "createdBy", "defaultHarnessProfileId", "evaluationId"}
	case "harness-definition":
		return []string{"id"}
	case "harness-version":
		return []string{"id", "definitionId"}
	case "harness-profile":
		return []string{"id", "workspaceId", "harnessVersionId"}
	}
	return nil
}

func compileDocumentSchemas() {
	documentSchemas = map[string]*jsonschema.Schema{}
	for _, source := range []struct {
		raw      string
		mappings map[string]string
	}{{agentSchemaJSON, map[string]string{"agent-version": "version"}}, {harnessSchemaJSON, map[string]string{"harness-definition": "definition", "harness-version": "version", "harness-profile": "profile"}}} {
		var root map[string]any
		decoder := json.NewDecoder(strings.NewReader(source.raw))
		decoder.UseNumber()
		if err := decoder.Decode(&root); err != nil {
			documentSchemaErr = err
			return
		}
		properties, ok := root["properties"].(map[string]any)
		if !ok {
			documentSchemaErr = errors.New("schema properties missing")
			return
		}
		for kind, property := range source.mappings {
			sub, ok := properties[property].(map[string]any)
			if !ok {
				documentSchemaErr = fmt.Errorf("schema %s missing", property)
				return
			}
			copy := make(map[string]any, len(sub)+2)
			for key, value := range sub {
				copy[key] = value
			}
			copy["$schema"] = "https://json-schema.org/draft/2020-12/schema"
			copy["$defs"] = root["$defs"]
			compiler := jsonschema.NewCompiler()
			compiler.AssertFormat()
			if err := compiler.AddResource("mem://"+kind, copy); err != nil {
				documentSchemaErr = err
				return
			}
			compiled, err := compiler.Compile("mem://" + kind)
			if err != nil {
				documentSchemaErr = err
				return
			}
			documentSchemas[kind] = compiled
		}
	}
}

func (s *Service) CreateAgent(ctx context.Context, name string, tags []string, key string) (client.AgentEnvelope, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.AgentEnvelope, error) {
		return s.api.CreateAgent(ctx, x.AccessToken, sel.WorkspaceID, key, client.CreateAgentRequest{Name: name, Tags: tags})
	})
}
func (s *Service) ListAgents(ctx context.Context, cursor string) (client.AgentList, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.AgentList, error) {
		return s.api.ListAgents(ctx, x.AccessToken, sel.WorkspaceID, cursor)
	})
}
func (s *Service) GetAgent(ctx context.Context, id string) (client.AgentEnvelope, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.AgentEnvelope, error) {
		return s.api.GetAgent(ctx, x.AccessToken, sel.WorkspaceID, id)
	})
}
func (s *Service) PublishAgent(ctx context.Context, id, key string, d client.JSONDocument) (client.PublishAgentVersionEnvelope, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.PublishAgentVersionEnvelope, error) {
		return s.api.PublishAgentVersion(ctx, x.AccessToken, sel.WorkspaceID, id, key, client.PublishAgentVersionRequest{Version: d})
	})
}
func (s *Service) ListAgentVersions(ctx context.Context, id, cursor string) (client.AgentVersionList, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.AgentVersionList, error) {
		return s.api.ListAgentVersions(ctx, x.AccessToken, sel.WorkspaceID, id, cursor)
	})
}
func (s *Service) GetAgentVersion(ctx context.Context, id, vid string) (client.AgentVersionEnvelope, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.AgentVersionEnvelope, error) {
		return s.api.GetAgentVersion(ctx, x.AccessToken, sel.WorkspaceID, id, vid)
	})
}
func (s *Service) CreateDefinition(ctx context.Context, key string, d client.JSONDocument) (client.HarnessDefinitionEnvelope, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.HarnessDefinitionEnvelope, error) {
		return s.api.CreateHarnessDefinition(ctx, x.AccessToken, sel.WorkspaceID, key, client.CreateHarnessDefinitionRequest{Definition: d})
	})
}
func (s *Service) ListDefinitions(ctx context.Context, cursor string) (client.HarnessDefinitionList, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.HarnessDefinitionList, error) {
		return s.api.ListHarnessDefinitions(ctx, x.AccessToken, sel.WorkspaceID, cursor)
	})
}
func (s *Service) GetDefinition(ctx context.Context, id string) (client.HarnessDefinitionEnvelope, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.HarnessDefinitionEnvelope, error) {
		return s.api.GetHarnessDefinition(ctx, x.AccessToken, sel.WorkspaceID, id)
	})
}
func (s *Service) PublishVersion(ctx context.Context, id, key string, d client.JSONDocument) (client.HarnessVersionEnvelope, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.HarnessVersionEnvelope, error) {
		return s.api.PublishHarnessVersion(ctx, x.AccessToken, sel.WorkspaceID, id, key, client.PublishHarnessVersionRequest{Version: d})
	})
}
func (s *Service) ListVersions(ctx context.Context, id, cursor string) (client.HarnessVersionList, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.HarnessVersionList, error) {
		return s.api.ListHarnessVersions(ctx, x.AccessToken, sel.WorkspaceID, id, cursor)
	})
}
func (s *Service) GetVersion(ctx context.Context, id, vid string) (client.HarnessVersionEnvelope, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.HarnessVersionEnvelope, error) {
		return s.api.GetHarnessVersion(ctx, x.AccessToken, sel.WorkspaceID, id, vid)
	})
}
func (s *Service) CreateProfile(ctx context.Context, key string, d client.JSONDocument) (client.HarnessProfileEnvelope, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.HarnessProfileEnvelope, error) {
		return s.api.CreateHarnessProfile(ctx, x.AccessToken, sel.WorkspaceID, key, client.CreateHarnessProfileRequest{Profile: d})
	})
}
func (s *Service) ListProfiles(ctx context.Context, cursor string) (client.HarnessProfileList, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.HarnessProfileList, error) {
		return s.api.ListHarnessProfiles(ctx, x.AccessToken, sel.WorkspaceID, cursor)
	})
}
func (s *Service) GetProfile(ctx context.Context, id string) (client.HarnessProfileEnvelope, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.HarnessProfileEnvelope, error) {
		return s.api.GetHarnessProfile(ctx, x.AccessToken, sel.WorkspaceID, id)
	})
}
func (s *Service) ReviseProfile(ctx context.Context, id, key string, expected int, d client.JSONDocument) (client.HarnessProfileEnvelope, error) {
	return call(ctx, s, func(sel workspacepkg.Selection, x workspacepkg.Session) (client.HarnessProfileEnvelope, error) {
		return s.api.ReviseHarnessProfile(ctx, x.AccessToken, sel.WorkspaceID, id, key, client.ReviseHarnessProfileRequest{Profile: d, ExpectedResourceVersion: expected})
	})
}
