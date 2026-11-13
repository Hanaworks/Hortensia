#!/usr/bin/env python3
"""Targeted correction and native file-entry characterization; see prior plan."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import time
import traceback
import uuid

NS = "dmnc_b_20261113b"
ROOT = Path("/data/docker-manager-native/run-20261113-nc2")
ROOT.mkdir(parents=True, exist_ok=False)
os.chmod(ROOT,0o700)
for name in ("home","docker-config"):
    (ROOT/name).mkdir(mode=0o700)
(ROOT/"empty.env").write_text("")
TOKEN="dmnc_fixture_"+uuid.uuid4().hex
BASE_ENV={"PATH":"/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin","HOME":str(ROOT/"home"),"DOCKER_CONFIG":str(ROOT/"docker-config"),"LANG":"C.UTF-8","LC_ALL":"C.UTF-8"}
IMAGE=json.loads(Path("/data/docker-manager-native/run-20261113-nc1/results.json").read_text())["environment"]["image_ref"]
PREFIX=[shutil.which("docker"),"--host","unix:///var/run/docker.sock"]
ALL={"namespace":NS,"root":str(ROOT),"cases":[]}
LOG=ROOT/"commands.jsonl"

def stamp(): return datetime.datetime.now(datetime.timezone.utc).isoformat()
def clean(x):
    if isinstance(x,str): return x.replace(TOKEN,"<redacted:fixture-secret>")
    if isinstance(x,dict): return {k:clean(v) for k,v in x.items()}
    if isinstance(x,list): return [clean(v) for v in x]
    return x
def log(x):
    with LOG.open("a") as f: f.write(json.dumps(clean(x),ensure_ascii=False)+"\n")
def run(args,*,cwd=ROOT,ok=(0,)):
    start=time.monotonic()
    p=subprocess.run(args,cwd=cwd,env=BASE_ENV|{"NC_IMAGE":IMAGE},capture_output=True,text=True,timeout=60)
    log({"started":stamp(),"argv":args,"cwd":str(cwd),"env_keys":sorted(BASE_ENV|{"NC_IMAGE":IMAGE}),"exit_code":p.returncode,"seconds":round(time.monotonic()-start,3),"stdout":p.stdout,"stderr":p.stderr})
    if ok is not None: assert p.returncode in ok,"unexpected command exit; see log"
    return p
def docker(*args,**kw): return run(PREFIX+list(args),**kw)
def inspect(kind,name): return json.loads(docker(kind,"inspect",name).stdout)[0]
def fixture(name,files):
    p=ROOT/name
    p.mkdir()
    for name,text in files.items():
        target=p/name
        target.parent.mkdir(parents=True,exist_ok=True)
        target.write_text(text)
        os.chmod(target,0o600)
    return p
def compose(p,name,*args,**kw):
    return run(PREFIX+["compose","--ansi","never","--project-name",NS+"_"+name,"--project-directory",str(p),"--env-file",str(ROOT/"empty.env"),"-f",str(p/"compose.yaml")]+list(args),cwd=p,**kw)
def ids(name,kind="container"):
    args=[kind,"ls"]+(["--all","--no-trunc"] if kind=="container" else ["--no-trunc"] if kind=="network" else [])
    return docker(*args,"--filter","label=com.docker.compose.project="+NS+"_"+name,"--format","{{.Name}}" if kind=="volume" else "{{.ID}}").stdout.split()
def save(): (ROOT/"results.json").write_text(json.dumps(clean(ALL),indent=2,ensure_ascii=False))
def case(n,fn):
    r={"id":n,"started":stamp()}
    ALL["cases"].append(r)
    try:
        r["evidence"]=fn()
        r["status"]="PASS"
    except Exception as e:
        r["status"]="FAIL"
        r["error"]=str(e)
        log({"case":n,"traceback":traceback.format_exc()})
    r["finished"]=stamp()
    save()
    print(n+" "+r["status"],flush=True)

def no_image():
    p=fixture("noimage",{"compose.yaml":"services:\n  app:\n    build: ./buildctx\n    command: ['true']\n","buildctx/Dockerfile":"FROM "+IMAGE+"\nLABEL dmnc.build_marker="+NS+"\n"})
    model=json.loads(compose(p,"noimage","config","--format","json").stdout)
    assert model["services"]["app"]["build"]["context"]==str(p/"buildctx")
    assert "image" not in model["services"]["app"]
    result=compose(p,"noimage","up","--detach","--no-build",ok=None)
    assert result.returncode!=0 and "must be a string" not in result.stderr
    assert any(s in result.stderr.lower() for s in ("no such image","not found","pull access denied")),result.stderr
    assert docker("image","ls","--filter","label=dmnc.build_marker="+NS,"--quiet").stdout.strip()==""
    return {"config_succeeded":True,"image_field_absent":True,"up_exit":result.returncode,"up_stderr":result.stderr,"created_container_ids":ids("noimage"),"created_network_ids":ids("noimage","network"),"build_marker_images":[]}

def external_volume():
    n,v=NS+"_external_net",NS+"_external_vol"
    p=fixture("external",{"compose.yaml":"services:\n  app:\n    image: ${NC_IMAGE}\n    command: [sh, -c, 'sleep 300']\n    networks: [ext]\n    volumes: [ext:/state]\nnetworks:\n  ext:\n    external: true\n    name: "+n+"\nvolumes:\n  ext:\n    external: true\n    name: "+v+"\n"})
    docker("network","create","--label","dmnc.test.owner="+NS,n)
    result=compose(p,"external","up","--detach","--no-build",ok=None)
    assert result.returncode!=0 and "volume" in result.stderr.lower() and "external" in result.stderr.lower()
    assert docker("volume","inspect",v,ok=None).returncode!=0
    docker("volume","create","--label","dmnc.test.owner="+NS,v)
    compose(p,"external","up","--detach","--no-build")
    assert len(ids("external"))==1
    assert ids("external","network")==[] and ids("external","volume")==[]
    return {"missing_volume_exit":result.returncode,"missing_volume_stderr":result.stderr,"after_create_container_ids":ids("external"),"managed_external_networks":[],"managed_external_volumes":[]}

def files():
    p=fixture("files",{"compose.yaml":"""x-container-env: &container-env
  env_file: [app.env]
services:
  app:
    <<: *container-env
    image: ${NC_IMAGE}
    command: [sh, -c, 'sleep 300']
    label_file: [first.labels, second.labels]
    labels:
      native.priority: inline
    configs:
      - source: fixture_config
        target: /fixture-config
    secrets:
      - fixture_secret
configs:
  fixture_config:
    file: ./config.txt
secrets:
  fixture_secret:
    file: ./secret.txt
""","app.env":"APP_SETTING=from-alias-env-file\nFIXTURE_TOKEN="+TOKEN+"\n","first.labels":"native.file=first\nnative.priority=first\n","second.labels":"native.file=second\nnative.priority=second\n","config.txt":"config fixture bytes\n","secret.txt":TOKEN+"\n"})
    before={str(f.relative_to(p)):hashlib.sha256(f.read_bytes()).hexdigest() for f in p.rglob("*") if f.is_file()}
    model=json.loads(compose(p,"files","config","--format","json").stdout)
    app=model["services"]["app"]
    assert app["environment"]["APP_SETTING"]=="from-alias-env-file"
    assert app["labels"]["native.file"]=="second" and app["labels"]["native.priority"]=="inline"
    assert model["configs"]["fixture_config"]["file"]==str(p/"config.txt")
    assert model["secrets"]["fixture_secret"]["file"]==str(p/"secret.txt")
    compose(p,"files","up","--detach","--no-build")
    ref=ids("files")[0]
    r=inspect("container",ref)
    actualenv=dict(v.split("=",1) for v in r["Config"]["Env"])
    assert actualenv["APP_SETTING"]=="from-alias-env-file" and actualenv["FIXTURE_TOKEN"]==TOKEN
    assert r["Config"]["Labels"]["native.file"]=="second" and r["Config"]["Labels"]["native.priority"]=="inline"
    mounts={v["Destination"]:v for v in r["Mounts"]}
    assert not mounts["/fixture-config"]["RW"] and not mounts["/run/secrets/fixture_secret"]["RW"]
    assert docker("exec",ref,"cat","/fixture-config").stdout=="config fixture bytes\n"
    assert docker("exec",ref,"cat","/run/secrets/fixture_secret").stdout==TOKEN+"\n"
    after={str(f.relative_to(p)):hashlib.sha256(f.read_bytes()).hexdigest() for f in p.rglob("*") if f.is_file()}
    assert before==after
    return {"original_sha256":before,"label_file_order":"second","inline_label_priority":"inline","alias_env_file_observed":True,"secret_file_content_asserted_redacted":True,"config_secret_readonly_mounts":mounts,"container_id":ref,"product_label_file_enabled":False}

def cleanup():
    result={"removed":{},"remaining":{}}
    for kind,fmt in (("container","{{.Names}}"),("network","{{.Name}}"),("volume","{{.Name}}")):
        args=[kind,"ls"]+(["--all"] if kind=="container" else [])+["--format",fmt]
        names=[n for n in docker(*args).stdout.split() if n.startswith(NS+"_")]
        result["removed"][kind]=names
        for n in names:
            assert n.startswith(NS+"_")
            docker(kind,"rm",*(["--force"] if kind=="container" else []),n)
        result["remaining"][kind]=[n for n in docker(*args).stdout.split() if n.startswith(NS+"_")]
        assert result["remaining"][kind]==[]
    return result

try:
    for kind,fmt in (("container","{{.Names}}"),("network","{{.Name}}"),("volume","{{.Name}}")):
        args=[kind,"ls"]+(["--all"] if kind=="container" else [])+["--format",fmt]
        assert not any(n.startswith(NS+"_") for n in docker(*args).stdout.split()),"existing namespace"
    case("NC05b",no_image)
    case("NC06b",external_volume)
    case("NC12",files)
finally:
    case("NC11b",cleanup)
    save()
    print(json.dumps({"results":str(ROOT/"results.json"),"log":str(LOG)}),flush=True)
