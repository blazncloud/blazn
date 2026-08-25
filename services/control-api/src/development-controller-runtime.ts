import type { DevelopmentBuildExecutor } from "./development-build-executor.js";
import { DevelopmentControllerService } from "./development-controller-service.js";
import type { DevelopmentControllerStore,DevelopmentControllerWorkItem } from "./development-controller-store.js";

export interface DevelopmentControllerRuntimeOptions {workerId:string;leaseSeconds:number;retryDelaySeconds:number;pollMilliseconds:number}
export type RuntimeEvent={type:"claimed"|"completed"|"retry"|"operational-failure"|"lease-lost";buildId:string;attempt:number};

export class DevelopmentControllerRuntime {
  private readonly service:DevelopmentControllerService;
  constructor(private readonly store:DevelopmentControllerStore,private readonly executor:DevelopmentBuildExecutor,
    private readonly options:DevelopmentControllerRuntimeOptions,private readonly event:(event:RuntimeEvent)=>void=()=>{}){
    this.service=new DevelopmentControllerService(store);
  }
  async runOnce(parentSignal:AbortSignal):Promise<boolean>{
    const item=await this.service.claim(this.options.workerId,this.options.leaseSeconds);if(!item)return false;
    this.event({type:"claimed",buildId:item.buildId,attempt:item.attempt});
    const controller=new AbortController(),abort=()=>controller.abort(parentSignal.reason);
    parentSignal.addEventListener("abort",abort,{once:true});
    if(parentSignal.aborted)abort();
    let renewing=false,leaseLost=false;
    const interval=setInterval(()=>{if(renewing||controller.signal.aborted)return;renewing=true;void this.renew(item).then(ok=>{if(!ok){leaseLost=true;controller.abort(new Error("Development controller lease was lost"));}}).catch(()=>{leaseLost=true;controller.abort(new Error("Development controller lease renewal failed"));}).finally(()=>{renewing=false;});},Math.max(1000,Math.floor(this.options.leaseSeconds*1000/3)));
    interval.unref();
    try{
      const result=await this.executor.execute(item,controller.signal);
      if(leaseLost||controller.signal.aborted)throw controller.signal.reason;
      const completed=await this.service.commitExecution(item.buildId,this.options.workerId,item.leaseToken,item.buildVersion,result.execution,result.document,result.artifacts);
      if(!completed)throw new Error("Development controller finalization was fenced");
      this.event({type:"completed",buildId:item.buildId,attempt:item.attempt});return true;
    }catch(error){
      if(leaseLost){this.event({type:"lease-lost",buildId:item.buildId,attempt:item.attempt});return true;}
      if(parentSignal.aborted)throw error;
      const released=await this.store.release(item.buildId,this.options.workerId,item.leaseToken,this.options.retryDelaySeconds,"controller_execution_failed");
      if(!released)this.event({type:"lease-lost",buildId:item.buildId,attempt:item.attempt});else this.event({type:item.attempt>=5?"operational-failure":"retry",buildId:item.buildId,attempt:item.attempt});
      return true;
    }finally{clearInterval(interval);parentSignal.removeEventListener("abort",abort);}
  }
  async run(signal:AbortSignal):Promise<void>{
    while(!signal.aborted){const worked=await this.runOnce(signal);if(!worked)await delay(this.options.pollMilliseconds,signal);}
  }
  private renew(item:DevelopmentControllerWorkItem){return this.service.renew(item.buildId,this.options.workerId,item.leaseToken,this.options.leaseSeconds).then(Boolean);}
}

function delay(milliseconds:number,signal:AbortSignal):Promise<void>{return new Promise((resolve,reject)=>{const abort=()=>{clearTimeout(timer);reject(signal.reason);},timer=setTimeout(()=>{signal.removeEventListener("abort",abort);resolve();},milliseconds);signal.addEventListener("abort",abort,{once:true});if(signal.aborted)abort();});}
