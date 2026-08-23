import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { Ajv2020 } from "ajv/dist/2020.js";
import type { FormatsPlugin } from "ajv-formats";

const here=path.dirname(fileURLToPath(import.meta.url)),require=createRequire(import.meta.url);
const formatsModule=require("ajv-formats") as {default?:FormatsPlugin}|FormatsPlugin;
const addFormats=("default" in formatsModule?formatsModule.default:formatsModule) as FormatsPlugin;
const contract=path.resolve(here,"../../../packages/contracts/plugin-broker-request.schema.json");
const responseContract=path.resolve(here,"../../../packages/contracts/plugin-broker-response.schema.json");
const requestId="a".repeat(32),runId="00000000-0000-4000-8000-000000000001",artifactId="00000000-0000-4000-8000-000000000002",draftId="00000000-0000-4000-8000-000000000003",digest=`sha256:${"b".repeat(64)}`;

test("plugin broker accepts only closed scoped request variants",async()=>{const schema=JSON.parse(await readFile(contract,"utf8")) as object;const ajv=new Ajv2020({strict:true,allErrors:true});addFormats(ajv);const validate=ajv.compile(schema);const requests=[
  {schemaVersion:1,requestId,method:"broker.describe",params:{}},
  {schemaVersion:1,requestId,method:"project.get",params:{}},
  {schemaVersion:1,requestId,method:"project.profile.get",params:{}},
  {schemaVersion:1,requestId,method:"project.profile.put",params:{profileSchemaVersion:"blazn.content/project/v1alpha1",draftId,artifactId,digest,status:"active",expectedVersion:0,idempotencyKey:"profile-put-1"}},
  {schemaVersion:1,requestId,method:"run.create",params:{kind:"content.render",proofClass:"synthetic",planDigest:digest,inputArtifactIds:[artifactId],outputNames:["preview.mp4"],idempotencyKey:"run-create-1"}},
  {schemaVersion:1,requestId,method:"run.list",params:{status:"all",cursor:""}},
  {schemaVersion:1,requestId,method:"run.get",params:{runId}},
  {schemaVersion:1,requestId,method:"run.cancel",params:{runId,expectedVersion:1,idempotencyKey:"run-cancel-1"}},
  {schemaVersion:1,requestId,method:"run.synthetic.progress",params:{runId,sequence:0,phase:"render.start",percent:0,message:"started"}},
  {schemaVersion:1,requestId,method:"run.synthetic.complete",params:{runId,expectedVersion:1,planDigest:digest,artifactIds:[artifactId],summary:{steps:1,warnings:[]},idempotencyKey:"run-complete-1"}},
  {schemaVersion:1,requestId,method:"artifact.list",params:{status:"ready"}},
  {schemaVersion:1,requestId,method:"artifact.get",params:{artifactId}},
  {schemaVersion:1,requestId,method:"artifact.upload.begin",params:{runId,name:"preview.mp4",kind:"content.video",mediaType:"video",sizeBytes:21,digest,idempotencyKey:"artifact-upload-1"}},
];for(const request of requests)assert.equal(validate(request),true,`${request.method}: ${JSON.stringify(validate.errors)}`);
});

test("plugin broker rejects authority injection, proof upgrade, secrets, and unsafe upload bounds",async()=>{const schema=JSON.parse(await readFile(contract,"utf8")) as object;const ajv=new Ajv2020({strict:true,allErrors:true});addFormats(ajv);const validate=ajv.compile(schema);const base={schemaVersion:1,requestId,method:"run.create",params:{kind:"content.render",proofClass:"synthetic",planDigest:digest,inputArtifactIds:[],outputNames:[],idempotencyKey:"run-create-1"}};for(const request of [
  {...base,workspaceId:"00000000-0000-4000-8000-000000000099"},
  {...base,params:{...base.params,proofClass:"provider"}},
  {...base,params:{...base.params,accessToken:"must-not-pass"}},
  {schemaVersion:1,requestId,method:"artifact.upload.begin",params:{runId,name:"preview.mp4",kind:"content.video",mediaType:"video",sizeBytes:1073741825,digest,idempotencyKey:"artifact-upload-1"}},
])assert.equal(validate(request),false,JSON.stringify(request));});

test("plugin broker response negotiation is closed and mutually exclusive",async()=>{const schema=JSON.parse(await readFile(responseContract,"utf8")) as object;const ajv=new Ajv2020({strict:true,allErrors:true});addFormats(ajv);const validate=ajv.compile(schema);const success={schemaVersion:1,requestId,ok:true,resultSchema:"run-envelope/v1",payload:"{\"run\":{}}"},failure={schemaVersion:1,requestId,ok:false,error:{code:"permission_denied",message:"request denied",retryable:false}};assert.equal(validate(success),true,JSON.stringify(validate.errors));assert.equal(validate(failure),true,JSON.stringify(validate.errors));assert.equal(validate({...success,error:failure.error}),false);assert.equal(validate({...success,resultSchema:"arbitrary/v1"}),false);assert.equal(validate({...failure,payload:"{}"}),false);});
