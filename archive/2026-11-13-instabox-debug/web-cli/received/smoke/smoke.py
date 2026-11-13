#!/usr/bin/env python3
"""Private, loopback-only actual executable/HTTPS/WS smoke; no fixture hooks."""
import base64
import fcntl
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import re
import secrets
import signal
import socket
import sqlite3
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

os.umask(0o077)
ROOT = Path('/data/docker-manager-debug/smoke')
SOURCE = Path('/data/docker-manager-debug/source')
RUN = ROOT / 'run-01'
RUN.mkdir(mode=0o700, parents=True, exist_ok=False)
E = RUN / 'evidence'
E.mkdir(mode=0o700)
OWNED = {}
LOGS = []
RESULTS = []

def write_json(path, obj):
    path.write_text(json.dumps(obj, indent=2) + '\n')

def record(name, **values):
    item = {'check': name, **values}
    RESULTS.append(item)
    with (E / 'assertions.jsonl').open('a') as f:
        f.write(json.dumps(item) + '\n')
    print(json.dumps(item), flush=True)

def run(name, args, stdin=None, timeout=60):
    with (E / (name+'.stdout')).open('wb') as out, (E / (name+'.stderr')).open('wb') as err:
        p = subprocess.run(args, input=stdin, stdout=out, stderr=err, cwd=SOURCE, timeout=timeout)
    (E / (name+'.exit')).write_text(str(p.returncode)+'\n')
    assert p.returncode == 0, name+' failed; private fixed diagnostic retained'

def start(name, args):
    out = (E / (name+'.stdout')).open('ab')
    err = (E / (name+'.stderr')).open('ab')
    LOGS.extend([out, err])
    p = subprocess.Popen(args, cwd=RUN, stdin=subprocess.DEVNULL, stdout=out, stderr=err)
    OWNED[name] = p
    (RUN / (name+'.pid')).write_text(str(p.pid)+'\n')
    return p

def stop(name):
    p = OWNED.get(name)
    if p is None:
        return
    if p.poll() is None:
        p.send_signal(signal.SIGTERM)
        try:
            p.wait(timeout=45)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait(timeout=5)
            raise AssertionError(name+' required forced shutdown')
    assert p.returncode == 0, name+' shutdown was not clean'
    record(name+'-shutdown', exit_code=p.returncode, pid_exited=True)
    del OWNED[name]

def lock_check(path, available):
    with path.open('a') as f:
        try:
            fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            got = False
        else:
            got = True
            fcntl.flock(f, fcntl.LOCK_UN)
    assert got == available, 'unexpected process lock ownership'

def ro_state(node):
    # Public ReadRuntimeConnectionState requires the live Store; OpenRuntimeService
    # would generate a new boot and revoke the running CLI. Do not open a writer.
    with sqlite3.connect((RUN/'manager-data/manager.db').as_uri()+'?mode=ro', uri=True) as db:
        db.execute('PRAGMA query_only=ON')
        row = db.execute('''SELECT n.frozen,n.ledger_id,n.session_id,s.status,s.total,s.seen,s.pages
          FROM nodes n JOIN recovery_nodes r ON r.node_id=n.id
          JOIN recovery_scans s ON s.id=r.active_round WHERE n.id=?''', (node,)).fetchone()
        count = db.execute('SELECT count(*) FROM operations WHERE node_id=?', (node,)).fetchone()[0]
    return row, count

def signal_exit(sig, frame):
    raise RuntimeError('controlled orchestration interrupted')

signal.signal(signal.SIGTERM, signal_exit)
signal.signal(signal.SIGINT, signal_exit)
success = False
try:
    (E/'started.txt').write_text(time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())+'\n')
    (E/'status').write_text('running\n')
    for folder in ('bin', 'tls', 'manager-data', 'manager-keys', 'agent-state', 'agent-identity', 'agent-keys'):
        (RUN/folder).mkdir(mode=0o700)
    run('build-manager', ['go','build','-o',str(RUN/'bin/manager'),'./cmd/manager'], timeout=180)
    run('build-agent', ['go','build','-o',str(RUN/'bin/agent'),'./cmd/agent'], timeout=180)
    record('official-cli-builds', manager_sha256=hashlib.sha256((RUN/'bin/manager').read_bytes()).hexdigest(), agent_sha256=hashlib.sha256((RUN/'bin/agent').read_bytes()).hexdigest())
    tls = RUN/'tls'
    run('create-ca', ['openssl','req','-x509','-newkey','rsa:2048','-sha256','-nodes','-days','2','-subj','/CN=dm-private-smoke-ca','-keyout',str(tls/'ca.key'),'-out',str(tls/'ca.pem')])
    run('create-server-csr', ['openssl','req','-newkey','rsa:2048','-nodes','-subj','/CN=127.0.0.1','-keyout',str(tls/'server.key'),'-out',str(tls/'server.csr')])
    (tls/'server.ext').write_text('subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\n')
    run('sign-server-cert', ['openssl','x509','-req','-in',str(tls/'server.csr'),'-CA',str(tls/'ca.pem'),'-CAkey',str(tls/'ca.key'),'-CAcreateserial','-out',str(tls/'server.pem'),'-days','2','-sha256','-extfile',str(tls/'server.ext')])
    with socket.socket() as s:
        s.bind(('127.0.0.1',0))
        port=s.getsockname()[1]
    origin='https://127.0.0.1:'+str(port)
    manager_config={'listen':'127.0.0.1:'+str(port),'external_origin':origin,'tls_cert_file':str(tls/'server.pem'),'tls_key_file':str(tls/'server.key'),'database_file':str(RUN/'manager-data/manager.db'),'keys_dir':str(RUN/'manager-keys'),'web_dir':str(SOURCE/'web/dist'),'log_level':'error'}
    write_json(RUN/'manager.json',manager_config)
    password=secrets.token_urlsafe(24)
    (RUN/'test-password').write_text(password+'\n')
    manager=[str(RUN/'bin/manager')]
    run('manager-init',manager+['init','--config',str(RUN/'manager.json'),'--admin','smoke-admin','--password-stdin'], stdin=(password+'\n').encode())
    start('manager-1',manager+['serve','--config',str(RUN/'manager.json')])
    ctx=ssl.create_default_context(cafile=str(tls/'ca.pem'))
    jar=http.cookiejar.CookieJar()
    client=urllib.request.build_opener(urllib.request.HTTPSHandler(context=ctx),urllib.request.HTTPCookieProcessor(jar))
    csrf=''
    def request(method,path,body=None,expected=200,match=None,key=None,raw=False):
        data=None if body is None else json.dumps(body).encode()
        headers={}
        if body is not None:
            headers['Content-Type']='application/json'
        if method!='GET':
            headers['Origin']=origin
            if csrf:
                headers['X-CSRF-Token']=csrf
            if path!='/api/v1/sessions':
                headers['Idempotency-Key']=key or str(uuid.uuid4())
        if match is not None:
            headers['If-Match']=match
        req=urllib.request.Request(origin+path,data=data,headers=headers,method=method)
        try:
            response=client.open(req,timeout=10)
        except urllib.error.HTTPError as error:
            response=error
        with response:
            result=response.read()
            if response.status!=expected:
                code='non-json'
                try: code=json.loads(result).get('error',{}).get('code','unknown')
                except (ValueError,AttributeError): pass
                raise AssertionError(method+' '+path+' HTTP '+str(response.status)+' code='+code+' expected '+str(expected))
            return (result if raw else json.loads(result)),response.headers
    deadline=time.monotonic()+20
    while True:
        try:
            page,_=request('GET','/',raw=True)
            break
        except (urllib.error.URLError,ConnectionError):
            assert time.monotonic()<deadline, 'manager not ready'
            assert OWNED['manager-1'].poll() is None, 'manager exited'
            time.sleep(.2)
    assert b'/assets/' in page, 'production Web not served'
    assets=re.findall(rb'(?:src|href)="(/assets/[^\"]+)"',page)
    assert len(assets)>=2
    for asset in assets:
        value,_=request('GET',asset.decode(),raw=True)
        assert len(value)>0
    record('production-web-served', index_status=200, local_asset_count=len(assets), loopback_only=True)
    try:
        urllib.request.urlopen(origin,timeout=5)
    except urllib.error.URLError as e:
        assert isinstance(e.reason,ssl.SSLCertVerificationError)
    else: raise AssertionError('untrusted test CA accepted')
    record('explicit-tls-trust', untrusted_ca_rejected=True, explicit_ca_succeeded=True, verification_disabled=False)
    def login():
        global csrf
        data,headers=request('POST','/api/v1/sessions',{'username':'smoke-admin','password':password})
        csrf=data['csrf_token']
        assert 'Secure' in headers['Set-Cookie'] and 'HttpOnly' in headers['Set-Cookie'] and '__Host-dm_session' in headers['Set-Cookie']
        reread,_=request('GET','/api/v1/me')
        assert reread['csrf_token']==csrf and reread['user_id']==data['user_id']
        return data['user_id']
    user=login()
    node=request('POST','/api/v1/nodes',{'display_name':'private A actual Agent'},expected=201)[0]['node_id']
    record('actual-login-node', secure_httponly_cookie=True, stable_session=True, node_id=node)
    # Admin is intentionally given no implicit node permissions.
    empty_bundle={'files':[{'path':'compose.yaml','content_b64':base64.b64encode(b'services: {}\n').decode()}],'compose_files':['compose.yaml'],'interpolation_env_files':[],'interpolation_env':{},'profiles':[]}
    denied,_=request('POST','/api/v1/nodes/'+node+'/stacks',{'project_name':'must_be_denied','config':empty_bundle},expected=403)
    assert denied['error']['code']=='permission_denied'
    _,headers=request('GET','/api/v1/users/'+user)
    caps=['node.read','config.read','config.write','stack.deploy','operation.read','operation.output.read']
    request('PUT','/api/v1/nodes/'+node+'/grants/'+user,{'capabilities':caps},match=headers['ETag'])
    login()  # grant revision invalidates the old browser session
    record('explicit-node-grants', before_grant_write_status=403, granted_count=len(caps), relogin=True)
    token=request('POST','/api/v1/nodes/'+node+'/enrollment-tokens',{},expected=201)[0]['token']
    agent_config={'manager_origin':origin,'ca_file':str(tls/'ca.pem'),'state_dir':str(RUN/'agent-state'),'identity_file':str(RUN/'agent-identity/identity.json'),'keys_dir':str(RUN/'agent-keys'),'docker_cli':'/usr/bin/docker','docker_socket':'unix:///var/run/docker.sock','execution_timeout_seconds':30,'log_level':'error'}
    write_json(RUN/'agent.json',agent_config)
    agent=[str(RUN/'bin/agent')]
    run('agent-init',agent+['init','--config',str(RUN/'agent.json'),'--node-id',node])
    run('agent-enroll',agent+['enroll','--config',str(RUN/'agent.json'),'--token-stdin'],stdin=(token+'\n').encode())
    token=None
    ident=json.loads((RUN/'agent-identity/identity.json').read_text())
    assert ident['credential_state']=='active' and ident['node_id']==node and ident['credential_generation']==1
    record('official-agent-enrolled', node_id=node,ledger_id=ident['ledger_id'],credential_state=ident['credential_state'],credential_generation=1)
    start('agent-1',agent+['serve','--config',str(RUN/'agent.json')])
    def await_online(label):
        deadline=time.monotonic()+45
        while True:
            view,_=request('GET','/api/v1/nodes/'+node)
            if view['status']=='online': break
            assert time.monotonic()<deadline, 'Agent never online; status='+view['status']
            time.sleep(.25)
        assert view['agent_capabilities']==[], 'empty allowlist advertised capabilities'
        row,count=ro_state(node)
        assert row and row[0]==0 and row[1]==ident['ledger_id'] and row[2] and row[3]=='completed' and row[4]==row[5]==0 and row[6]>=1
        assert count==0
        profile=view['execution_profile']
        assert profile, 'real Docker probe missing'
        record(label, status=view['status'], frozen=False, recovery_status=row[3], total=row[4], seen=row[5], pages=row[6], agent_capability_count=0, operation_count=count, execution_profile=profile)
    await_online('actual-ws-empty-recovery')
    lock_check(RUN/'manager-data/.manager.lock',False)
    lock_check(RUN/'agent-state/.agent.lock',False)
    record('running-locks', manager_exclusive=True, agent_exclusive=True)
    originals={'compose.yaml':'services:\n  web:\n    image: busybox:1.37\n    env_file:\n      - app.env\n    configs:\n      - source: sample\n        target: /etc/sample.txt\nconfigs:\n  sample:\n    file: ./config/sample.txt\n', 'overrides/dev.yaml':'services:\n  web:\n    command: ["echo", "${MESSAGE}"]\n', 'interpolation.env':'MESSAGE=private-smoke\n', 'app.env':'NON_SECRET=test-only\r\n', 'config/sample.txt':'private UTF-8 验证\n'}
    def bundle(files):
        return {'files':[{'path':path,'content_b64':base64.b64encode(content.encode()).decode()} for path,content in files.items()],'compose_files':['compose.yaml','overrides/dev.yaml'],'interpolation_env_files':['interpolation.env'],'interpolation_env':{},'profiles':[]}
    b1=bundle(originals)
    key=str(uuid.uuid4())
    body={'project_name':'actual_cli_smoke','config':b1}
    created,_=request('POST','/api/v1/nodes/'+node+'/stacks',body,expected=201,key=key)
    replay,_=request('POST','/api/v1/nodes/'+node+'/stacks',body,expected=201,key=key)
    assert replay==created
    stack=created['stack_id']; version1=created['version_id']
    def reread(version,want):
        result,_=request('GET','/api/v1/stacks/'+stack+'/versions/'+version)
        assert result['version_id']==version and result['config']==want, 'configuration bytes/order changed'
    reread(version1,b1)
    edited=dict(originals); edited['config/sample.txt']='private UTF-8 已更新\n'
    b2=bundle(edited)
    updated,_=request('POST','/api/v1/stacks/'+stack+'/versions',{'config':b2},expected=201,match='"'+version1+'"')
    version2=updated['version_id']
    assert version1!=version2
    reread(version1,b1); reread(version2,b2)
    record('actual-http-multifile-save',file_count=len(originals),versions=2,original_utf8_crlf_preserved=True,compose_order_preserved=True,idempotent_create=True,old_version_immutable=True,stack_id=stack)
    def rejected_apply(label):
        before,_=request('GET','/api/v1/operations?node_id='+node)
        reject,_=request('POST','/api/v1/stacks/'+stack+'/apply',{'version_id':version2},expected=422)
        assert reject['error']['code']=='unsupported'
        after,_=request('GET','/api/v1/operations?node_id='+node)
        assert before['items']==after['items']==[]
        row,count=ro_state(node)
        assert count==0
        record(label,http_status=422,error_code='unsupported',operation_count_before=0,operation_count_after=0)
    rejected_apply('empty-capabilities-apply')
    # Stop and reopen official programs to exercise persisted materials and locks.
    stop('agent-1'); stop('manager-1')
    lock_check(RUN/'manager-data/.manager.lock',True); lock_check(RUN/'agent-state/.agent.lock',True)
    start('manager-2',manager+['serve','--config',str(RUN/'manager.json')])
    deadline=time.monotonic()+20
    while True:
        try:
            request('GET','/api/v1/me')
            break
        except (urllib.error.URLError,ConnectionError):
            assert time.monotonic()<deadline
            time.sleep(.2)
    reread(version1,b1); reread(version2,b2)
    start('agent-2',agent+['serve','--config',str(RUN/'agent.json')])
    await_online('actual-cli-restart-empty-recovery')
    rejected_apply('restart-empty-capabilities-apply')
    record('actual-cli-restart',same_session=True,persisted_versions=2,same_node_and_ledger=True)
    success=True
except BaseException as error:
    # Assertion text only; never include HTTP response bodies or secret structures.
    record('failure',error_type=type(error).__name__,message=str(error))
finally:
    cleanup_ok=True
    for name in list(OWNED)[::-1]:
        try:
            stop(name)
        except BaseException as error:
            cleanup_ok=False
            record('cleanup-failure',service=name,error_type=type(error).__name__)
    for log in LOGS: log.close()
    try:
        lock_check(RUN/'manager-data/.manager.lock',True)
        lock_check(RUN/'agent-state/.agent.lock',True)
        lingering=[]
        for proc in Path('/proc').iterdir():
            if proc.name.isdigit() and int(proc.name)!=os.getpid():
                try: cmd=(proc/'cmdline').read_bytes()
                except (OSError,ProcessLookupError): continue
                if str(RUN/'bin').encode() in cmd and (b'manager' in cmd or b'agent' in cmd): lingering.append(int(proc.name))
        assert not lingering, 'owned service/supervisor still running'
        record('final-cleanup',owned_service_count=0,owned_supervisor_count=0,manager_lock_reacquired=True,agent_lock_reacquired=True,docker_daemon_touched=False)
    except BaseException as error:
        cleanup_ok=False
        record('cleanup-failure',error_type=type(error).__name__)
    success=success and cleanup_ok
    (E/'finished.txt').write_text(time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime())+'\n')
    (E/'overall.exit').write_text(('0' if success else '1')+'\n')
    (E/'status').write_text('finished\n')
    sys.exit(0 if success else 1)

