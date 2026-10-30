#!/usr/bin/python3
"""Linux account E2E provider; never connects to a real provider or keyring."""
import os,sys,json,pathlib,time,uuid
provider=pathlib.Path(sys.argv[0]).name
root=pathlib.Path(os.environ['ACCOUNT_E2E_ROOT']); home=pathlib.Path(os.environ.get('CODEX_HOME' if provider=='codex' else 'CLAUDE_CONFIG_DIR') or pathlib.Path.home()/('.codex' if provider=='codex' else '.claude'))
credential=home/('auth.json' if provider=='codex' else '.credentials.json');args=sys.argv[1:]
email=json.loads(credential.read_text()).get('fixtureEmail','device-'+provider+'@example.test') if credential.is_file() else 'device-'+provider+'@example.test'
def audit(action,conversation=None):
 if '/agent-accounts/' in str(home):
  for key in (('OPENAI_API_KEY','CODEX_API_KEY','CODEX_ACCESS_TOKEN') if provider=='codex' else ('ANTHROPIC_API_KEY','ANTHROPIC_AUTH_TOKEN','CLAUDE_CODE_OAUTH_TOKEN')):
   if os.environ.get(key):raise RuntimeError('ambient credential reached managed provider')
 with(root/'invocations.jsonl').open('a')as f:f.write(json.dumps({'provider':provider,'identity':email,'action':action,'conversationId':conversation})+'\n')
def output(value):print(json.dumps(value),flush=True)
def identity():return {'type':'chatgpt','email':email,'planType':'plus'} if credential.is_file() else None
def transcript(tid,cwd):
 d=home/'sessions';d.mkdir(parents=True,exist_ok=True);p=d/('rollout-'+tid+'.jsonl')
 if not p.exists():p.write_text(json.dumps({'timestamp':'2026-10-30T00:00:00Z','type':'session_meta','payload':{'id':tid,'cwd':cwd,'timestamp':'2026-10-30T00:00:00Z','cli_version':'0.165.0'}})+'\n'+json.dumps({'type':'response_item','payload':{'type':'message','role':'user','content':[{'type':'input_text','text':'Fake preserved history'}]}})+'\n')
if '--version' in args or '-v' in args:print('codex-cli 0.165.0' if provider=='codex' else '2.1.0 (Claude Code)');sys.exit(0)
if '--help' in args:print('Fixture provider');sys.exit(0)
if 'generate-json-schema' in args:
 out=pathlib.Path(args[args.index('--out')+1]);out.mkdir(parents=True,exist_ok=True);(out/'fixture.json').write_text(json.dumps({'enum':['account/read','account/rateLimits/read','account/updated','model/list']}));sys.exit(0)
if 'login' in args:
 audit('login');print('token=ACCOUNT_E2E_PRIVATE_TOKEN_CANARY',flush=True);print('client_secret=ACCOUNT_E2E_PRIVATE_TOKEN_CANARY',flush=True);print('\x1b]8;;https://example.test/?token=ACCOUNT_E2E_PRIVATE_TOKEN_CANARY\x1b\\Sign in\x1b]8;;\x1b\\',flush=True);print('ac\x1b[32mcess_token\x1b[0m=ACCOUNT_E2E_PRIVATE_TOKEN_CANARY',flush=True);print('\x1b]8;;https://example.test/?access_token=ACCOUNT_E2E_PRIVATE_TOKEN_CANARY\x1b\\Sign in\x1b]8;;\x1b\\',flush=True);print('Fake subscription sign-in waiting.',flush=True);print('Paste authorization code: ',end='',flush=True)
 while not(root/'approve-login').exists():time.sleep(.02)
 home.mkdir(mode=0o700,parents=True,exist_ok=True);(home/'.fixture-identity').write_text((root/'next-identity').read_text());(home/'.fixture-identity').chmod(0o600);credential.write_text(json.dumps({'fakeCredential':'ACCOUNT_E2E_PRIVATE_TOKEN_CANARY','fixtureEmail':(root/'next-identity').read_text().strip()}));credential.chmod(0o600)
 if(root/'hold-login').exists():
  (root/'credential-written').write_text(str(os.getpid()))
  while(root/'hold-login').exists():time.sleep(.02)
 (root/'login-finalized').write_text('complete')
 print('Fake sign-in completed.',end='',flush=True);sys.exit(0)
if 'logout' in args:audit('logout');credential.unlink(missing_ok=True);sys.exit(0)
if provider=='claude' and 'status' in args:audit('identity');output({'loggedIn':credential.exists(),'authMethod':'claude.ai','email':email,'subscriptionType':'max'});sys.exit(0)
if provider=='claude' and '--output-format' in args:
 audit('usage')
 for line in sys.stdin:
  try:m=json.loads(line)
  except ValueError:continue
  if m.get('type')=='control_request':output({'type':'control_response','response':{'subtype':'success','request_id':m.get('request_id'),'response':{'usage':{}}}})
 sys.exit(0)
if 'app-server' in args:
 audit('app-server')
 for line in sys.stdin:
  try:m=json.loads(line)
  except ValueError:continue
  if 'id'not in m:continue
  method=m.get('method');params=m.get('params')or{}
  if method=='account/read':result={'account':identity(),'requiresOpenaiAuth':True}
  elif method=='account/logout':credential.unlink(missing_ok=True);result={};audit('logout')
  elif method=='account/rateLimits/read':result={'rateLimits':{'primary':{'usedPercent':20,'windowDurationMins':300,'resetsAt':int(time.time())+3600},'planType':'plus'}}
  elif method=='model/list':result={'data':[{'id':'gpt-fake','model':'gpt-fake','displayName':'GPT Fake','isDefault':True,'supportedReasoningEfforts':[{'reasoningEffort':'low','description':'Low'}],'defaultReasoningEffort':'low'}],'nextCursor':None}
  elif method in ('thread/start','thread/resume','thread/read'):
   tid=params.get('threadId')or str(uuid.uuid4())
   if method=='thread/resume' and not (home/'sessions'/('rollout-'+tid+'.jsonl')).exists():
    output({'id':m['id'],'error':{'code':-32602,'message':'Fixture conversation unavailable'}});continue
   if method=='thread/start' and not params.get('ephemeral'):transcript(tid,params.get('cwd')or os.getcwd())
   result={'thread':{'id':tid,'turns':[]}};audit(method+'-ephemeral' if params.get('ephemeral') else method,tid)
  elif method=='turn/start':tid=params.get('threadId');result={'turn':{'id':'fake-turn','status':'inProgress'}};audit('turn')
  else:result={}
  output({'id':m['id'],'result':result})
  if method=='turn/start':output({'method':'turn/completed','params':{'threadId':tid,'turn':{'id':'fake-turn','status':'completed','items':[]}}})
 sys.exit(0)
audit('terminal');print('Fake terminal ready',flush=True)
while True:time.sleep(1)
