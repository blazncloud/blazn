import { createHash } from "node:crypto";

export interface DevelopmentImageChildren {"linux/amd64":string;"linux/arm64":string}
type Fetcher=(input:string,init:RequestInit)=>Promise<Response>;
interface ResolverOptions {allowedAuthority:string;fetcher?:Fetcher}
const accepted=["application/vnd.oci.image.index.v1+json","application/vnd.docker.distribution.manifest.list.v2+json"];
const childMediaTypes=["application/vnd.oci.image.manifest.v1+json","application/vnd.docker.distribution.manifest.v2+json"];

export async function resolveDevelopmentImageChildren(reference:string,signal:AbortSignal,options:ResolverOptions):Promise<DevelopmentImageChildren>{
  const parsed=parseReference(reference),allowed=parseAuthority(options.allowedAuthority);if(parsed.authority!==allowed)throw new Error("Development registry authority is not allowlisted");
  const deadline=AbortSignal.timeout(30_000),combined=AbortSignal.any([signal,deadline]),fetcher=options.fetcher??fetch;
  const endpoint=`https://${parsed.authority}/v2/${parsed.repository}/manifests/${parsed.digest}`;
  const response=await fetcher(endpoint,{method:"GET",headers:{accept:accepted.join(", ")},redirect:"manual",signal:combined,credentials:"omit",referrerPolicy:"no-referrer"});
  if(response.status!==200)throw new Error(`Development registry inspection returned HTTP ${response.status}`);
  if(response.url&&response.url!==endpoint)throw new Error("Development registry inspection changed origin or path");
  const contentType=(response.headers.get("content-type")??"").split(";",1)[0]!.trim().toLowerCase();
  if(!accepted.includes(contentType))throw new Error("Development registry returned an unsupported index media type");
  const body=await readBounded(response,1024*1024),observed=`sha256:${createHash("sha256").update(body).digest("hex")}`;
  if(observed!==parsed.digest)throw new Error("Development registry index digest is invalid");
  let value:unknown;try{value=JSON.parse(body.toString("utf8"));}catch{throw new Error("Development registry index is invalid JSON");}
  const root=record(value),manifests=root?.manifests;if(!root||root.schemaVersion!==2||!accepted.includes(text(root.mediaType))||!Array.isArray(manifests)||manifests.length<2||manifests.length>64)throw new Error("Development registry index shape is invalid");
  const children=new Map<string,string>();
  for(const raw of manifests){const item=record(raw),platform=record(item?.platform),architecture=text(platform?.architecture),os=text(platform?.os),digest=text(item?.digest),size=item?.size;
    if(os!=="linux"||!['amd64','arm64'].includes(architecture))continue;
    const name=`linux/${architecture}`;if(children.has(name)||!childMediaTypes.includes(text(item?.mediaType))||!digestPattern.test(digest)||!Number.isSafeInteger(size)||Number(size)<1)throw new Error("Development registry child descriptor is invalid");
    children.set(name,`${parsed.authority}/${parsed.repository}@${digest}`);
  }
  if(children.size!==2||!children.has("linux/amd64")||!children.has("linux/arm64"))throw new Error("Development registry index lacks the exact platform set");
  return{"linux/amd64":children.get("linux/amd64")!,"linux/arm64":children.get("linux/arm64")!};
}

function parseReference(value:string){const match=/^([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]{1,5})?)\/([a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:\/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*)@(sha256:[0-9a-f]{64})$/.exec(value);if(!match)throw new Error("Development image index reference is invalid");return{authority:parseAuthority(match[1]!),repository:match[2]!,digest:match[3]!};}
function parseAuthority(authority:string){const match=/^([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?)(?::([0-9]{1,5}))?$/.exec(authority);if(!match)throw new Error("Development registry authority is invalid");const host=match[1]!,port=match[2];if(!host.includes(".")||/^\d+(?:\.\d+){3}$/.test(host)||port&&(!/^[1-9][0-9]{0,4}$/.test(port)||Number(port)>65535))throw new Error("Development registry authority is invalid");return authority;}
async function readBounded(response:Response,maximum:number){const length=response.headers.get("content-length");if(length&&(!/^[0-9]+$/.test(length)||Number(length)>maximum))throw new Error("Development registry index is too large");if(!response.body)throw new Error("Development registry index body is missing");const reader=response.body.getReader(),chunks:Uint8Array[]=[];let size=0;for(;;){const item=await reader.read();if(item.done)break;size+=item.value.byteLength;if(size>maximum){await reader.cancel();throw new Error("Development registry index is too large");}chunks.push(item.value);}return Buffer.concat(chunks.map(value=>Buffer.from(value)),size);}
function record(value:unknown):Record<string,unknown>|undefined{return value&&typeof value==="object"&&!Array.isArray(value)?value as Record<string,unknown>:undefined;}
function text(value:unknown){return typeof value==="string"?value:"";}
const digestPattern=/^sha256:[0-9a-f]{64}$/;
