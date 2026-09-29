"""Disposable local-container database only; never accepts an external database URL."""
import subprocess,time,json,os
from pathlib import Path
contracts=Path(os.environ["BLAZN_QUALIFICATION_CONTRACTS"]).resolve()
assert (contracts/"contracts/projects.openapi.json").is_file(), "contracts directory is required"
D=['sudo','-n','docker'];pg='blazn-m2-disposable-20260928';runner='blazn-m2-runner-20260928'
def run(args,**kw):return subprocess.run(D+args,check=True,**kw)
for name in [pg,runner]:
 result=subprocess.run(D+['inspect',name],capture_output=True)
 assert result.returncode!=0,'refusing to reuse existing container'
try:
 run(['run','-d','--name',pg,'--network','none','--cpus','0.5','--memory','512m','--tmpfs','/var/lib/postgresql/data:rw,size=512m','-e','POSTGRES_HOST_AUTH_METHOD=trust','postgres:16-alpine@sha256:cf78e76683b9ca8c5733cbbdce6c9262b45b6767934dd0a95e671f9a0fc20685'],stdout=subprocess.DEVNULL)
 for attempt in range(40):
  if subprocess.run(D+['exec',pg,'pg_isready','-U','postgres'],capture_output=True).returncode==0:break
  time.sleep(.5)
 else:raise RuntimeError('disposable PostgreSQL unavailable')
 roles=['blazn_migration','blazn_runtime','blazn_bootstrap','blazn_node_broker','blazn_sandbox_controller','blazn_development_controller','blazn_agent_run_controller']
 sql='\n'.join('CREATE ROLE '+r+' LOGIN;' for r in roles)+'\nCREATE DATABASE blazn_qualification OWNER blazn_migration;\n'
 run(['exec','-i',pg,'psql','-U','postgres','-v','ON_ERROR_STOP=1'],input=sql.encode(),stdout=subprocess.DEVNULL)
 run(['run','-d','--name',runner,'--network','container:'+pg,'--cpus','1','--memory','1536m','--tmpfs','/tmp:rw,size=64m','--mount',f'type=bind,src={contracts},dst=/packages,readonly','blazn-m2-qualification:20260928','sleep','600'],stdout=subprocess.DEVNULL)
 setup="require('fs').writeFileSync('/tmp/migration-url','postgresql://blazn_migration@127.0.0.1:5432/blazn_qualification')"
 run(['exec',runner,'node','-e',setup])
 run(['exec','-e','MIGRATION_DATABASE_URL_FILE=/tmp/migration-url',runner,'node','dist/migrate.js'])
 env=[]
 for name in ['WORKSPACE','PROJECT']:
  env+=['-e',f'BLAZN_{name}_TEST_DATABASE_URL=postgresql://blazn_runtime@127.0.0.1:5432/blazn_qualification','-e',f'BLAZN_{name}_TEST_ADMIN_DATABASE_URL=postgresql://postgres@127.0.0.1:5432/blazn_qualification']
 tests=['workspace-policy','workspace-http','workspace-store.integration','project-contract-validation','project-http','project-service','project-store.integration','oidc','oidc-state','auth-page','browser-cors']
 run(['exec',*env,runner,'node','--test','--test-concurrency=1',*[f'dist/{t}.test.js' for t in tests]])
 print(json.dumps({'disposable_db':True,'network':'none/shared-container-loopback','migration_files':38,'existing_databases_contacted':False}))
finally:
 for name in [runner,pg]:subprocess.run(D+['rm','-f',name],capture_output=True)
 print('Task containers and tmpfs database removed.')
