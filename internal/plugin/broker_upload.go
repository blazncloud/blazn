package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"os"

	"github.com/blazncloud/blazn/internal/client"
)

const brokerMaxArtifactBytes int64 = 16 * 1024 * 1024
const brokerMaxUploadFrames = 4096

type brokerArtifactUploadParams struct {
	RunID          string `json:"runId"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	MediaType      string `json:"mediaType"`
	SizeBytes      *int64 `json:"sizeBytes"`
	Digest         string `json:"digest"`
	IdempotencyKey string `json:"idempotencyKey"`
}
type brokerArtifactUploadReady struct {
	MaxDataBytes     int    `json:"maxDataBytes"`
	ExpectedSizeBytes int64  `json:"expectedSizeBytes"`
	ExpectedDigest   string `json:"expectedDigest"`
}
type brokerArtifactUpload struct {
	requestID string
	runID string
	workspaceID string
	projectID string
	name string
	digest string
	expected int64
	received int64
	frames int
	file *os.File
	path string
	hasher hash.Hash
	finish func(context.Context, io.ReadSeeker) (client.ArtifactEnvelope,error)
}

func (h *authenticatedBrokerHandler) beginArtifactUpload(ctx context.Context, pluginName string, runtimeContext RuntimeContext, request brokerRequest) (brokerArtifactUploadReady,*brokerArtifactUpload,*brokerError) {
	if pluginName!="content"||runtimeContext!=h.runtimeContext||runtimeContext.Status!="selected"||runtimeContext.ProjectID=="" { return brokerArtifactUploadReady{},nil,brokerMethodFailure("broker_context_unavailable","an authenticated Workspace Project is required",false) }
	var params brokerArtifactUploadParams
	if decodeBrokerParams(request.Params,&params)!=nil||!validBrokerArtifactUploadParams(params) { return brokerArtifactUploadReady{},nil,brokerMethodFailure("invalid_request","broker Artifact upload params are invalid",false) }
	if *params.SizeBytes>brokerMaxArtifactBytes { return brokerArtifactUploadReady{},nil,brokerMethodFailure("upload_too_large","synthetic Artifact exceeds the root upload policy",false) }
	h.once.Do(func(){ h.authority,h.initializeErr=h.initialize(runtimeContext) })
	if h.initializeErr!=nil||h.authority==nil { return brokerArtifactUploadReady{},nil,brokerMethodFailure("broker_context_unavailable","authenticated broker authority is unavailable",false) }
	run,err:=brokerWithSession(ctx,h.authority,runtimeContext,func(token string)(client.RunEnvelope,error){ return h.authority.api.GetRun(ctx,token,runtimeContext.WorkspaceID,runtimeContext.ProjectID,params.RunID) })
	if err!=nil { return brokerArtifactUploadReady{},nil,mapBrokerError(err) }
	if !validBrokerRun(run.Run,runtimeContext)||run.Run.ID!=params.RunID||run.Run.ProofClass!=client.ProofClassSynthetic||run.Run.Status!=client.RunStatusRunning||run.Run.RequestedBy!=runtimeContext.UserID||!containsString(run.Run.OutputNames,params.Name) { return brokerArtifactUploadReady{},nil,brokerMethodFailure("resource_unavailable","selected Run is not accepting this Artifact",false) }
	file,err:=os.CreateTemp(h.uploadTempDir,".blazn-artifact-upload-*")
	if err!=nil { return brokerArtifactUploadReady{},nil,brokerMethodFailure("broker_backend_unavailable","Artifact staging is unavailable",true) }
	if err:=file.Chmod(0o600);err!=nil { path:=file.Name();_ = file.Close();_ = os.Remove(path);return brokerArtifactUploadReady{},nil,brokerMethodFailure("broker_backend_unavailable","Artifact staging is unavailable",true) }
	metadata:=client.ArtifactUploadMetadata{Name:params.Name,Kind:params.Kind,MediaType:client.ArtifactMediaType(params.MediaType),SizeBytes:*params.SizeBytes,Digest:params.Digest}
	key:=scopedBrokerIdempotencyKey(runtimeContext,pluginName,request)
	upload:=&brokerArtifactUpload{requestID:request.RequestID,runID:params.RunID,workspaceID:runtimeContext.WorkspaceID,projectID:runtimeContext.ProjectID,name:params.Name,digest:params.Digest,expected:*params.SizeBytes,file:file,path:file.Name(),hasher:sha256.New()}
	upload.finish=func(callCtx context.Context,content io.ReadSeeker)(client.ArtifactEnvelope,error){ return brokerWithSession(callCtx,h.authority,runtimeContext,func(token string)(client.ArtifactEnvelope,error){ if _,err:=content.Seek(0,io.SeekStart);err!=nil{return client.ArtifactEnvelope{},err};return h.authority.api.UploadSyntheticRunArtifact(callCtx,token,runtimeContext.WorkspaceID,runtimeContext.ProjectID,params.RunID,key,metadata,content) }) }
	ready:=brokerArtifactUploadReady{MaxDataBytes:brokerMaxDataBytes,ExpectedSizeBytes:*params.SizeBytes,ExpectedDigest:params.Digest}
	return ready,upload,nil
}

func validBrokerArtifactUploadParams(value brokerArtifactUploadParams) bool { return brokerUUIDPattern.MatchString(value.RunID)&&len(value.Name)>=1&&len(value.Name)<=256&&brokerRunKindPattern.MatchString(value.Kind)&&(value.MediaType=="image"||value.MediaType=="video"||value.MediaType=="audio"||value.MediaType=="document"||value.MediaType=="data"||value.MediaType=="other")&&value.SizeBytes!=nil&&*value.SizeBytes>=0&&*value.SizeBytes<=1073741824&&brokerDigestPattern.MatchString(value.Digest)&&validBrokerIdempotencyKey(value.IdempotencyKey) }
func containsString(values []string,wanted string) bool { for _,value:=range values{if value==wanted{return true}};return false }
func (u *brokerArtifactUpload) write(payload []byte) error { if len(payload)==0||u.frames>=brokerMaxUploadFrames||u.received+int64(len(payload))>u.expected{return io.ErrShortWrite};u.frames++;if _,err:=u.file.Write(payload);err!=nil{return err};if _,err:=u.hasher.Write(payload);err!=nil{return err};u.received+=int64(len(payload));return nil }
func (u *brokerArtifactUpload) complete(ctx context.Context)(client.ArtifactEnvelope,*brokerError){ defer u.abort();if u.received!=u.expected||"sha256:"+hex.EncodeToString(u.hasher.Sum(nil))!=u.digest{return client.ArtifactEnvelope{},brokerMethodFailure("artifact_digest_mismatch","Artifact bytes do not match declared size and digest",false)};value,err:=u.finish(ctx,u.file);if err!=nil{return client.ArtifactEnvelope{},mapBrokerError(err)};if value.Artifact.ID==""||value.Artifact.WorkspaceID!=u.workspaceID||value.Artifact.ProjectID!=u.projectID||value.Artifact.SourceRunID!=u.runID||value.Artifact.Name!=u.name||value.Artifact.Status!=client.ArtifactStatusReady||value.Artifact.Digest!=u.digest||value.Artifact.SizeBytes==nil||*value.Artifact.SizeBytes!=u.expected{return client.ArtifactEnvelope{},brokerMethodFailure("broker_response_invalid","root API returned an invalid scoped Artifact",false)};return value,nil }
func (u *brokerArtifactUpload) abort(){ if u.file!=nil{_ = u.file.Close();u.file=nil};if u.path!=""{_ = os.Remove(u.path);u.path=""} }
