// Code generated from packages/contracts/agent-harness.openapi.json; DO NOT EDIT.
// Contract SHA256: 5bf03a0404891f55155109afd4559bbf2003e5596c904bf6a82bf4db7fab303a
package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
)

type JSONDocument map[string]any
type Agent struct {
	ID               string   `json:"id"`
	WorkspaceID      string   `json:"workspaceId"`
	OwnerID          string   `json:"ownerId"`
	Name             string   `json:"name"`
	Tags             []string `json:"tags"`
	Status           string   `json:"status"`
	CurrentVersionID *string  `json:"currentVersionId"`
	Version          int      `json:"version"`
	CreatedAt        string   `json:"createdAt"`
	UpdatedAt        string   `json:"updatedAt"`
}
type AgentVersion struct {
	ID          string       `json:"id"`
	AgentID     string       `json:"agentId"`
	WorkspaceID string       `json:"workspaceId"`
	Version     int          `json:"version"`
	Digest      string       `json:"digest"`
	Document    JSONDocument `json:"document"`
	CreatedBy   string       `json:"createdBy"`
	CreatedAt   string       `json:"createdAt"`
}
type HarnessDefinition struct {
	ID              string       `json:"id"`
	WorkspaceID     string       `json:"workspaceId"`
	Kind            string       `json:"kind"`
	Status          string       `json:"status"`
	ResourceVersion int          `json:"resourceVersion"`
	Document        JSONDocument `json:"document"`
	CreatedAt       string       `json:"createdAt"`
	UpdatedAt       string       `json:"updatedAt"`
}
type HarnessVersion struct {
	ID           string       `json:"id"`
	DefinitionID string       `json:"definitionId"`
	WorkspaceID  string       `json:"workspaceId"`
	Version      string       `json:"version"`
	Digest       string       `json:"digest"`
	Document     JSONDocument `json:"document"`
	CreatedBy    string       `json:"createdBy"`
	CreatedAt    string       `json:"createdAt"`
}
type HarnessProfile struct {
	ID               string       `json:"id"`
	WorkspaceID      string       `json:"workspaceId"`
	Name             string       `json:"name"`
	HarnessVersionID string       `json:"harnessVersionId"`
	Status           string       `json:"status"`
	ResourceVersion  int          `json:"resourceVersion"`
	Digest           string       `json:"digest"`
	Document         JSONDocument `json:"document"`
	CreatedAt        string       `json:"createdAt"`
	UpdatedAt        string       `json:"updatedAt"`
}
type AgentEnvelope struct {
	Agent Agent `json:"agent"`
}
type AgentList struct {
	Items      []Agent `json:"items"`
	NextCursor *string `json:"nextCursor"`
}
type AgentVersionEnvelope struct {
	Version AgentVersion `json:"version"`
}
type AgentVersionList struct {
	Items      []AgentVersion `json:"items"`
	NextCursor *string        `json:"nextCursor"`
}
type PublishAgentVersionEnvelope struct {
	Agent   Agent        `json:"agent"`
	Version AgentVersion `json:"version"`
}
type HarnessDefinitionEnvelope struct {
	Definition HarnessDefinition `json:"definition"`
}
type HarnessDefinitionList struct {
	Items      []HarnessDefinition `json:"items"`
	NextCursor *string             `json:"nextCursor"`
}
type HarnessVersionEnvelope struct {
	Version HarnessVersion `json:"version"`
}
type HarnessVersionList struct {
	Items      []HarnessVersion `json:"items"`
	NextCursor *string          `json:"nextCursor"`
}
type HarnessProfileEnvelope struct {
	Profile HarnessProfile `json:"profile"`
}
type HarnessProfileList struct {
	Items      []HarnessProfile `json:"items"`
	NextCursor *string          `json:"nextCursor"`
}
type CreateAgentRequest struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}
type PublishAgentVersionRequest struct {
	Version JSONDocument `json:"version"`
}
type CreateHarnessDefinitionRequest struct {
	Definition JSONDocument `json:"definition"`
}
type PublishHarnessVersionRequest struct {
	Version JSONDocument `json:"version"`
}
type CreateHarnessProfileRequest struct {
	Profile JSONDocument `json:"profile"`
}
type ReviseHarnessProfileRequest struct {
	Profile                 JSONDocument `json:"profile"`
	ExpectedResourceVersion int          `json:"expectedResourceVersion"`
}

var agentHarnessUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func (c *Client) agentHarnessDo(ctx context.Context, method, path, token, key string, query url.Values, body, out any, status int) error {
	return c.workspaceDo(ctx, method, path, token, key, query, body, out, status)
}
func ahBase(w string) (string, error) {
	if !agentHarnessUUID.MatchString(w) {
		return "", fmt.Errorf("workspace ID must be a UUID")
	}
	return workspaceResourcePath(w), nil
}
func ahResource(w, collection, id string) (string, error) {
	p, e := ahBase(w)
	if e != nil {
		return "", e
	}
	if !agentHarnessUUID.MatchString(id) {
		return "", fmt.Errorf("resource ID must be a UUID")
	}
	return p + collection + "/" + url.PathEscape(id), nil
}
func ahQuery(cursor string) (url.Values, error) {
	if len(cursor) > 128 {
		return nil, fmt.Errorf("cursor is invalid")
	}
	q := make(url.Values)
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	return q, nil
}
func (c *Client) CreateAgent(ctx context.Context, t, w, key string, r CreateAgentRequest) (AgentEnvelope, error) {
	var o AgentEnvelope
	p, e := ahBase(w)
	if e != nil {
		return o, e
	}
	if r.Name == "" || len(r.Name) > 96 || len(r.Tags) > 32 {
		return o, fmt.Errorf("Agent request is invalid")
	}
	e = c.agentHarnessDo(ctx, http.MethodPost, p+"/agents", t, key, nil, r, &o, http.StatusCreated)
	return o, e
}
func (c *Client) ListAgents(ctx context.Context, t, w, cursor string) (AgentList, error) {
	var o AgentList
	p, e := ahBase(w)
	if e != nil {
		return o, e
	}
	q, e := ahQuery(cursor)
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodGet, p+"/agents", t, "", q, nil, &o, http.StatusOK)
	}
	return o, e
}
func (c *Client) GetAgent(ctx context.Context, t, w, id string) (AgentEnvelope, error) {
	var o AgentEnvelope
	p, e := ahResource(w, "/agents", id)
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodGet, p, t, "", nil, nil, &o, http.StatusOK)
	}
	return o, e
}
func (c *Client) PublishAgentVersion(ctx context.Context, t, w, id, key string, r PublishAgentVersionRequest) (PublishAgentVersionEnvelope, error) {
	var o PublishAgentVersionEnvelope
	p, e := ahResource(w, "/agents", id)
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodPost, p+"/versions", t, key, nil, r, &o, http.StatusCreated)
	}
	return o, e
}
func (c *Client) ListAgentVersions(ctx context.Context, t, w, id, cursor string) (AgentVersionList, error) {
	var o AgentVersionList
	p, e := ahResource(w, "/agents", id)
	q, qe := ahQuery(cursor)
	if e == nil {
		e = qe
	}
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodGet, p+"/versions", t, "", q, nil, &o, http.StatusOK)
	}
	return o, e
}
func (c *Client) GetAgentVersion(ctx context.Context, t, w, id, vid string) (AgentVersionEnvelope, error) {
	var o AgentVersionEnvelope
	p, e := ahResource(w, "/agents", id)
	if e == nil && !agentHarnessUUID.MatchString(vid) {
		e = fmt.Errorf("version ID must be a UUID")
	}
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodGet, p+"/versions/"+url.PathEscape(vid), t, "", nil, nil, &o, http.StatusOK)
	}
	return o, e
}
func (c *Client) CreateHarnessDefinition(ctx context.Context, t, w, key string, r CreateHarnessDefinitionRequest) (HarnessDefinitionEnvelope, error) {
	var o HarnessDefinitionEnvelope
	p, e := ahBase(w)
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodPost, p+"/harness/definitions", t, key, nil, r, &o, http.StatusCreated)
	}
	return o, e
}
func (c *Client) ListHarnessDefinitions(ctx context.Context, t, w, cursor string) (HarnessDefinitionList, error) {
	var o HarnessDefinitionList
	p, e := ahBase(w)
	q, qe := ahQuery(cursor)
	if e == nil {
		e = qe
	}
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodGet, p+"/harness/definitions", t, "", q, nil, &o, http.StatusOK)
	}
	return o, e
}
func (c *Client) GetHarnessDefinition(ctx context.Context, t, w, id string) (HarnessDefinitionEnvelope, error) {
	var o HarnessDefinitionEnvelope
	p, e := ahResource(w, "/harness/definitions", id)
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodGet, p, t, "", nil, nil, &o, http.StatusOK)
	}
	return o, e
}
func (c *Client) PublishHarnessVersion(ctx context.Context, t, w, id, key string, r PublishHarnessVersionRequest) (HarnessVersionEnvelope, error) {
	var o HarnessVersionEnvelope
	p, e := ahResource(w, "/harness/definitions", id)
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodPost, p+"/versions", t, key, nil, r, &o, http.StatusCreated)
	}
	return o, e
}
func (c *Client) ListHarnessVersions(ctx context.Context, t, w, id, cursor string) (HarnessVersionList, error) {
	var o HarnessVersionList
	p, e := ahResource(w, "/harness/definitions", id)
	q, qe := ahQuery(cursor)
	if e == nil {
		e = qe
	}
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodGet, p+"/versions", t, "", q, nil, &o, http.StatusOK)
	}
	return o, e
}
func (c *Client) GetHarnessVersion(ctx context.Context, t, w, id, vid string) (HarnessVersionEnvelope, error) {
	var o HarnessVersionEnvelope
	p, e := ahResource(w, "/harness/definitions", id)
	if e == nil && !agentHarnessUUID.MatchString(vid) {
		e = fmt.Errorf("version ID must be a UUID")
	}
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodGet, p+"/versions/"+url.PathEscape(vid), t, "", nil, nil, &o, http.StatusOK)
	}
	return o, e
}
func (c *Client) CreateHarnessProfile(ctx context.Context, t, w, key string, r CreateHarnessProfileRequest) (HarnessProfileEnvelope, error) {
	var o HarnessProfileEnvelope
	p, e := ahBase(w)
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodPost, p+"/harness/profiles", t, key, nil, r, &o, http.StatusCreated)
	}
	return o, e
}
func (c *Client) ListHarnessProfiles(ctx context.Context, t, w, cursor string) (HarnessProfileList, error) {
	var o HarnessProfileList
	p, e := ahBase(w)
	q, qe := ahQuery(cursor)
	if e == nil {
		e = qe
	}
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodGet, p+"/harness/profiles", t, "", q, nil, &o, http.StatusOK)
	}
	return o, e
}
func (c *Client) GetHarnessProfile(ctx context.Context, t, w, id string) (HarnessProfileEnvelope, error) {
	var o HarnessProfileEnvelope
	p, e := ahResource(w, "/harness/profiles", id)
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodGet, p, t, "", nil, nil, &o, http.StatusOK)
	}
	return o, e
}
func (c *Client) ReviseHarnessProfile(ctx context.Context, t, w, id, key string, r ReviseHarnessProfileRequest) (HarnessProfileEnvelope, error) {
	var o HarnessProfileEnvelope
	p, e := ahResource(w, "/harness/profiles", id)
	if r.ExpectedResourceVersion < 1 {
		return o, fmt.Errorf("expected resource version must be positive")
	}
	if e == nil {
		e = c.agentHarnessDo(ctx, http.MethodPost, p+"/revisions", t, key, nil, r, &o, http.StatusOK)
	}
	return o, e
}
