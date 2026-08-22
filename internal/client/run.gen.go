// Code generated from packages/contracts/runs.openapi.json; DO NOT EDIT.
// Contract SHA256: 12f696f1c0121d3c34a3be489d9cf8baef34c50d6e66bb8e7da2ce4f01a404ff

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
)

type ProofClass string

const (
	ProofClassSynthetic ProofClass = "synthetic"
	ProofClassLocal     ProofClass = "local"
	ProofClassSandbox   ProofClass = "sandbox"
	ProofClassProvider  ProofClass = "provider"
)

type RunStatus string

const (
	RunStatusQueued    RunStatus = "queued"
	RunStatusRunning   RunStatus = "running"
	RunStatusSucceeded RunStatus = "succeeded"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCancelled RunStatus = "cancelled"
)

type RunReceiptOutcome string

const (
	RunReceiptOutcomeSucceeded RunReceiptOutcome = "succeeded"
	RunReceiptOutcomeFailed    RunReceiptOutcome = "failed"
	RunReceiptOutcomeCancelled RunReceiptOutcome = "cancelled"
)

type ArtifactStatus string

const (
	ArtifactStatusPending ArtifactStatus = "pending"
	ArtifactStatusReady   ArtifactStatus = "ready"
	ArtifactStatusFailed  ArtifactStatus = "failed"
	ArtifactStatusDeleted ArtifactStatus = "deleted"
)

type ArtifactMediaType string

const (
	ArtifactMediaTypeImage    ArtifactMediaType = "image"
	ArtifactMediaTypeVideo    ArtifactMediaType = "video"
	ArtifactMediaTypeAudio    ArtifactMediaType = "audio"
	ArtifactMediaTypeDocument ArtifactMediaType = "document"
	ArtifactMediaTypeData     ArtifactMediaType = "data"
	ArtifactMediaTypeOther    ArtifactMediaType = "other"
)

type RunPlacement struct {
	NodeID       string `json:"nodeId,omitempty"`
	SandboxID    string `json:"sandboxId,omitempty"`
	ModelRouteID string `json:"modelRouteId,omitempty"`
}
type RunReceiptSummary struct {
	Steps    int      `json:"steps"`
	Warnings []string `json:"warnings"`
}
type RunReceipt struct {
	SchemaVersion string            `json:"schemaVersion"`
	ProofClass    ProofClass        `json:"proofClass"`
	Outcome       RunReceiptOutcome `json:"outcome"`
	PlanDigest    string            `json:"planDigest"`
	ArtifactIDs   []string          `json:"artifactIds"`
	Summary       RunReceiptSummary `json:"summary"`
}
type Run struct {
	ID               string        `json:"id"`
	WorkspaceID      string        `json:"workspaceId"`
	ProjectID        string        `json:"projectId"`
	Kind             string        `json:"kind"`
	ProofClass       ProofClass    `json:"proofClass"`
	Status           RunStatus     `json:"status"`
	Version          int           `json:"version"`
	PlanDigest       string        `json:"planDigest"`
	InputArtifactIDs []string      `json:"inputArtifactIds"`
	OutputNames      []string      `json:"outputNames"`
	RequestedBy      string        `json:"requestedBy"`
	Placement        *RunPlacement `json:"placement"`
	Receipt          *RunReceipt   `json:"receipt"`
	CreatedAt        string        `json:"createdAt"`
	StartedAt        string        `json:"startedAt,omitempty"`
	CompletedAt      string        `json:"completedAt,omitempty"`
	ErrorCode        string        `json:"errorCode,omitempty"`
}
type RunEnvelope struct {
	Run Run `json:"run"`
}
type RunList struct {
	Items      []Run   `json:"items"`
	NextCursor *string `json:"nextCursor"`
}
type CreateRunRequest struct {
	Kind             string     `json:"kind"`
	ProofClass       ProofClass `json:"proofClass"`
	PlanDigest       string     `json:"planDigest"`
	InputArtifactIDs []string   `json:"inputArtifactIds"`
	OutputNames      []string   `json:"outputNames"`
}
type CancelRunRequest struct {
	ExpectedVersion int `json:"expectedVersion"`
}
type Artifact struct {
	ID                string            `json:"id"`
	WorkspaceID       string            `json:"workspaceId"`
	ProjectID         string            `json:"projectId"`
	SourceRunID       string            `json:"sourceRunId,omitempty"`
	Kind              string            `json:"kind"`
	MediaType         ArtifactMediaType `json:"mediaType"`
	Name              string            `json:"name"`
	Status            ArtifactStatus    `json:"status"`
	Version           int               `json:"version"`
	Digest            string            `json:"digest,omitempty"`
	SizeBytes         *int64            `json:"sizeBytes,omitempty"`
	CreatedBy         string            `json:"createdBy"`
	CreatedAt         string            `json:"createdAt"`
	UpdatedAt         string            `json:"updatedAt"`
	DownloadAvailable bool              `json:"downloadAvailable"`
}
type ArtifactEnvelope struct {
	Artifact Artifact `json:"artifact"`
}
type ArtifactList struct {
	Items      []Artifact `json:"items"`
	NextCursor *string    `json:"nextCursor"`
}
type RunError = ErrorBody

var runUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
var runKind = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,95}$`)
var runDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var runOutputName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (c *Client) CreateRun(ctx context.Context, accessToken, workspaceID, projectID, idempotencyKey string, request CreateRunRequest) (RunEnvelope, error) {
	var output RunEnvelope
	path, err := runCollectionPath(workspaceID, projectID)
	if err != nil {
		return output, err
	}
	if err = validateCreateRun(request); err != nil {
		return output, err
	}
	err = c.workspaceDo(ctx, http.MethodPost, path, accessToken, idempotencyKey, nil, request, &output, http.StatusAccepted)
	return output, err
}
func (c *Client) ListRuns(ctx context.Context, accessToken, workspaceID, projectID, status, cursor string) (RunList, error) {
	var output RunList
	path, err := runCollectionPath(workspaceID, projectID)
	if err != nil {
		return output, err
	}
	if status != "" && !validRunStatus(status, true) {
		return output, fmt.Errorf("Run status filter is invalid")
	}
	if len(cursor) > 512 {
		return output, fmt.Errorf("Run cursor is invalid")
	}
	query := make(url.Values)
	if status != "" {
		query.Set("status", status)
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	err = c.workspaceDo(ctx, http.MethodGet, path, accessToken, "", query, nil, &output, http.StatusOK)
	return output, err
}
func (c *Client) GetRun(ctx context.Context, accessToken, workspaceID, projectID, runID string) (RunEnvelope, error) {
	var output RunEnvelope
	path, err := runResourcePath(workspaceID, projectID, runID)
	if err != nil {
		return output, err
	}
	err = c.workspaceDo(ctx, http.MethodGet, path, accessToken, "", nil, nil, &output, http.StatusOK)
	return output, err
}
func (c *Client) CancelRun(ctx context.Context, accessToken, workspaceID, projectID, runID, idempotencyKey string, request CancelRunRequest) (RunEnvelope, error) {
	var output RunEnvelope
	path, err := runResourcePath(workspaceID, projectID, runID)
	if err != nil {
		return output, err
	}
	if request.ExpectedVersion < 1 {
		return output, fmt.Errorf("Run cancel request is invalid")
	}
	err = c.workspaceDo(ctx, http.MethodPost, path+"/cancel", accessToken, idempotencyKey, nil, request, &output, http.StatusOK)
	return output, err
}
func (c *Client) ListArtifacts(ctx context.Context, accessToken, workspaceID, projectID, status, cursor string) (ArtifactList, error) {
	var output ArtifactList
	path, err := artifactCollectionPath(workspaceID, projectID)
	if err != nil {
		return output, err
	}
	if status != "" && !validArtifactStatus(status, true) {
		return output, fmt.Errorf("Artifact status filter is invalid")
	}
	if len(cursor) > 512 {
		return output, fmt.Errorf("Artifact cursor is invalid")
	}
	query := make(url.Values)
	if status != "" {
		query.Set("status", status)
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	err = c.workspaceDo(ctx, http.MethodGet, path, accessToken, "", query, nil, &output, http.StatusOK)
	return output, err
}
func (c *Client) GetArtifact(ctx context.Context, accessToken, workspaceID, projectID, artifactID string) (ArtifactEnvelope, error) {
	var output ArtifactEnvelope
	path, err := artifactCollectionPath(workspaceID, projectID)
	if err != nil {
		return output, err
	}
	if !runUUID.MatchString(artifactID) {
		return output, fmt.Errorf("Artifact ID must be a UUID")
	}
	err = c.workspaceDo(ctx, http.MethodGet, path+"/"+url.PathEscape(artifactID), accessToken, "", nil, nil, &output, http.StatusOK)
	return output, err
}

func validateCreateRun(request CreateRunRequest) error {
	if !runKind.MatchString(request.Kind) || !validProofClass(request.ProofClass) || !runDigest.MatchString(request.PlanDigest) || len(request.InputArtifactIDs) > 1000 || len(request.OutputNames) > 1000 {
		return fmt.Errorf("Run create request is invalid")
	}
	ids := map[string]struct{}{}
	for _, id := range request.InputArtifactIDs {
		if !runUUID.MatchString(id) {
			return fmt.Errorf("Run input Artifact ID is invalid")
		}
		if _, ok := ids[id]; ok {
			return fmt.Errorf("Run input Artifact IDs must be unique")
		}
		ids[id] = struct{}{}
	}
	names := map[string]struct{}{}
	for _, name := range request.OutputNames {
		if !runOutputName.MatchString(name) {
			return fmt.Errorf("Run output name is invalid")
		}
		if _, ok := names[name]; ok {
			return fmt.Errorf("Run output names must be unique")
		}
		names[name] = struct{}{}
	}
	return nil
}
func validProofClass(value ProofClass) bool {
	return value == ProofClassSynthetic || value == ProofClassLocal || value == ProofClassSandbox || value == ProofClassProvider
}
func validRunStatus(value string, all bool) bool {
	return value == string(RunStatusQueued) || value == string(RunStatusRunning) || value == string(RunStatusSucceeded) || value == string(RunStatusFailed) || value == string(RunStatusCancelled) || (all && value == "all")
}
func validArtifactStatus(value string, all bool) bool {
	return value == string(ArtifactStatusPending) || value == string(ArtifactStatusReady) || value == string(ArtifactStatusFailed) || value == string(ArtifactStatusDeleted) || (all && value == "all")
}
func runProjectPath(workspaceID, projectID string) (string, error) {
	if !runUUID.MatchString(workspaceID) {
		return "", fmt.Errorf("Workspace ID must be a UUID")
	}
	if !runUUID.MatchString(projectID) {
		return "", fmt.Errorf("Project ID must be a UUID")
	}
	return workspaceResourcePath(workspaceID) + "/projects/" + url.PathEscape(projectID), nil
}
func runCollectionPath(workspaceID, projectID string) (string, error) {
	path, err := runProjectPath(workspaceID, projectID)
	if err != nil {
		return "", err
	}
	return path + "/runs", nil
}
func runResourcePath(workspaceID, projectID, runID string) (string, error) {
	path, err := runCollectionPath(workspaceID, projectID)
	if err != nil {
		return "", err
	}
	if !runUUID.MatchString(runID) {
		return "", fmt.Errorf("Run ID must be a UUID")
	}
	return path + "/" + url.PathEscape(runID), nil
}
func artifactCollectionPath(workspaceID, projectID string) (string, error) {
	path, err := runProjectPath(workspaceID, projectID)
	if err != nil {
		return "", err
	}
	return path + "/artifacts", nil
}
