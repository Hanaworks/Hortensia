#!/usr/bin/env python3
"""Disposable native CLI characterization, no product code or allowlist changes."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import time
import traceback
import uuid

NS = "dmnc_b_20261113a"
ROOT = Path("/data/docker-manager-native/run-20261113-nc1")
ROOT.mkdir(parents=True, exist_ok=False)
os.chmod(ROOT, 0o700)
LOG = ROOT / "commands.jsonl"
RESULT = ROOT / "results.json"
TOKEN = "dmnc_fixture_" + uuid.uuid4().hex
DOCKER = shutil.which("docker")
for name in ("home", "docker-config"):
    (ROOT / name).mkdir(mode=0o700)
EMPTY = ROOT / "empty.env"
EMPTY.write_text("")
BASE_ENV = {
    "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "HOME": str(ROOT / "home"), "DOCKER_CONFIG": str(ROOT / "docker-config"),
    "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8",
}
ALL = {"namespace": NS, "root": str(ROOT), "started": None, "environment": {}, "cases": [], "cleanup": {}}
PREFIX = [DOCKER, "--host", "unix:///var/run/docker.sock"]

def stamp():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()

def clean(value):
    if isinstance(value, str):
        return value.replace(TOKEN, "<redacted:fixture-secret>")
    if isinstance(value, dict):
        return {k: clean(v) for k, v in value.items()}
    if isinstance(value, list):
        return [clean(v) for v in value]
    return value

def log(value):
    with LOG.open("a") as f:
        f.write(json.dumps(clean(value), ensure_ascii=False) + "\n")

def save():
    RESULT.write_text(json.dumps(clean(ALL), indent=2, ensure_ascii=False))

def run(argv, *, cwd=ROOT, env=None, timeout=60, ok=(0,)):
    started = stamp()
    started_mono = time.monotonic()
    p = subprocess.run(argv, cwd=cwd, env=BASE_ENV | (env or {}), capture_output=True, text=True, timeout=timeout)
    log({"started": started, "argv": argv, "cwd": str(cwd), "env_keys": sorted((BASE_ENV | (env or {})).keys()),
         "exit_code": p.returncode, "seconds": round(time.monotonic()-started_mono, 3), "stdout": p.stdout, "stderr": p.stderr})
    if ok is not None and p.returncode not in ok:
        raise AssertionError("unexpected exit %s for %s (see commands.jsonl)" % (p.returncode, argv))
    return p

def docker(*args, **kw):
    return run(PREFIX + list(args), **kw)

def jdocker(*args, **kw):
    return json.loads(docker(*args, **kw).stdout)

def project(name):
    return NS + "_" + name

def fixture(name, files):
    p = ROOT / name
    p.mkdir()
    for rel, text in files.items():
        target = p / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text)
        os.chmod(target, 0o600)
    return p

def compose_argv(p, name, action, *, files=("compose.yaml",), envfiles=None, profiles=()):
    args = PREFIX + ["compose", "--ansi", "never", "--project-name", project(name), "--project-directory", str(p)]
    chosen_envs = [EMPTY] if envfiles is None else [p / n for n in envfiles]
    for f in chosen_envs:
        args += ["--env-file", str(f)]
    for f in files:
        args += ["-f", str(p / f)]
    for profile in profiles:
        args += ["--profile", profile]
    return args + list(action)

def compose(p, name, *action, **kw):
    env = {"NC_IMAGE": ALL["environment"]["image_ref"], "FIXTURE_TOKEN": TOKEN}
    env |= kw.pop("env", {})
    return run(compose_argv(p, name, action, **{k:kw.pop(k) for k in list(kw) if k in ("files", "envfiles", "profiles")}), cwd=p, env=env, **kw)

def model(p, name, **kw):
    return json.loads(compose(p, name, "config", "--format", "json", **kw).stdout)

def ids(name, kind="container"):
    args = [kind, "ls"]
    if kind == "container": args += ["--all", "--no-trunc"]
    if kind == "network": args += ["--no-trunc"]
    args += ["--filter", "label=com.docker.compose.project=" + project(name), "--format", "{{.Name}}" if kind == "volume" else "{{.ID}}"]
    return docker(*args).stdout.split()

def resources(name):
    result = {}
    for kind in ("container", "network", "volume"):
        result[kind] = [jdocker(kind, "inspect", ref)[0] for ref in ids(name, kind)]
    return result

def hashes(p):
    return {str(f.relative_to(p)):hashlib.sha256(f.read_bytes()).hexdigest() for f in sorted(p.rglob("*")) if f.is_file()}

def case(n, fn):
    entry = {"id": n, "started": stamp()}
    ALL["cases"].append(entry)
    try:
        entry["evidence"] = fn()
        entry["status"] = "PASS"
    except Exception as e:
        entry["status"] = "FAIL"
        entry["error"] = clean(str(e))
        log({"case": n, "error": str(e), "traceback": traceback.format_exc()})
    entry["finished"] = stamp()
    save()
    print(n + " " + entry["status"], flush=True)

def setup():
    ALL["started"] = stamp()
    ALL["environment"]["uname"] = run(["uname", "-a"]).stdout.strip()
    ALL["environment"]["os_release"] = Path("/etc/os-release").read_text()
    ALL["environment"]["id"] = run(["id"]).stdout.strip()
    ALL["environment"]["docker_cli"] = DOCKER
    ALL["environment"]["docker_version"] = jdocker("version", "--format", "{{json .}}")
    info = jdocker("info", "--format", "{{json .}}")
    ALL["environment"]["engine_id"] = info["ID"]
    ALL["environment"]["driver"] = info["Driver"]
    ALL["environment"]["compose_version"] = docker("compose", "version", "--short").stdout.strip()
    for kind, fmt in (("container", "{{.Names}}"), ("network", "{{.Name}}"), ("volume", "{{.Name}}")):
        args = [kind, "ls"] + (["--all"] if kind == "container" else []) + ["--format", fmt]
        assert not any(x.startswith(NS) for x in docker(*args).stdout.split()), "namespace already exists"
    docker("pull", "busybox:1.37.0", timeout=150)
    image = jdocker("image", "inspect", "busybox:1.37.0")[0]
    ref = next(x for x in image["RepoDigests"] if x.startswith("busybox@sha256:"))
    ALL["environment"] |= {"image_ref": ref, "image_id": image["Id"], "image_architecture": image["Architecture"], "image_repo_digests": image["RepoDigests"]}
    return {"image_ref": ref, "image_id": image["Id"], "compose": ALL["environment"]["compose_version"]}

MAIN = None
MAIN_KW = {"files": ("compose.yaml", "sub/override.yaml"), "envfiles": ("first.env", "second.env"), "profiles": ("hidden",), "env": {"MODE": "from-map"}}

def config_cases():
    global MAIN
    MAIN = fixture("main", {
        "compose.yaml": """services:
  app:
    image: ${NC_IMAGE}
    command: [sh, -c, 'sleep 300']
    environment:
      MERGE_KEY: base
      ORDER: ${ORDER:-default_order}
      MODE: ${MODE:-default_mode}
      FIXTURE_TOKEN: ${FIXTURE_TOKEN}
      HOST_SENTINEL: ${HOST_SENTINEL:-absent}
    env_file: [app.env]
    volumes:
      - ./config/data.txt:/fixture/data.txt:ro
      - data:/state
    labels:
      native.fixture.merge: base
  dormant:
    image: ${NC_IMAGE}
    command: [sh, -c, 'sleep 300']
    profiles: [hidden]
volumes:
  data: {}
""",
        "sub/override.yaml": """services:
  app:
    environment:
      MERGE_KEY: override
    labels:
      native.fixture.merge: override
    volumes:
      - ./config/data.txt:/fixture/second.txt:ro
""",
        "config/data.txt": "native fixture bytes\n",
        "app.env": "APP_SETTING=from-container-env\nDOCKER_HOST=container-fake-only\n",
        ".env": "ORDER=dot_env\nMODE=dot_mode\n",
        "first.env": "ORDER=first\nMODE=first\n",
        "second.env": "ORDER=second\nMODE=second\n",
    })
    before = hashes(MAIN)
    normal = model(MAIN, "main", **MAIN_KW)
    assert set(normal["services"]) == {"app", "dormant"}
    app = normal["services"]["app"]
    assert app["environment"]["MERGE_KEY"] == "override"
    assert app["environment"]["ORDER"] == "second"
    assert app["environment"]["MODE"] == "from-map"
    assert app["environment"]["FIXTURE_TOKEN"] == TOKEN
    assert app["environment"]["HOST_SENTINEL"] == "absent"
    assert app["environment"]["APP_SETTING"] == "from-container-env"
    binds = [v for v in app["volumes"] if v["type"] == "bind"]
    assert len(binds) == 2 and all(v["source"] == str(MAIN / "config/data.txt") and v["read_only"] for v in binds)
    reverse = model(MAIN, "main", files=("sub/override.yaml", "compose.yaml"), envfiles=("second.env", "first.env"))
    assert reverse["services"]["app"]["environment"]["MERGE_KEY"] == "base"
    assert reverse["services"]["app"]["environment"]["ORDER"] == "first"
    assert "dormant" not in reverse["services"]
    empty = model(MAIN, "main")
    assert empty["services"]["app"]["environment"]["ORDER"] == "default_order"
    dot = model(MAIN, "main", envfiles=(".env",))
    assert dot["services"]["app"]["environment"]["ORDER"] == "dot_env"
    assert hashes(MAIN) == before
    return {"sha256_before_after": before, "normal_model": normal, "reverse_merge_key": "base", "reverse_env_order": "first", "empty_env_order": "default_order", "selected_dotenv_order": "dot_env", "implicit_host_sentinel": "absent"}

def apply_main():
    assert MAIN is not None
    before = hashes(MAIN)
    compose(MAIN, "main", "up", "--detach", "--no-build", **MAIN_KW)
    snapshot = resources("main")
    assert len(snapshot["container"]) == 2
    assert len(snapshot["network"]) == 1 and len(snapshot["volume"]) == 1
    assert snapshot["network"][0]["Name"] == project("main") + "_default"
    assert snapshot["volume"][0]["Name"] == project("main") + "_data"
    app = next(x for x in snapshot["container"] if x["Config"]["Labels"]["com.docker.compose.service"] == "app")
    actualenv = dict(x.split("=", 1) for x in app["Config"]["Env"])
    for k, v in {"MERGE_KEY":"override", "ORDER":"second", "MODE":"from-map", "APP_SETTING":"from-container-env", "FIXTURE_TOKEN":TOKEN, "DOCKER_HOST":"container-fake-only", "HOST_SENTINEL":"absent"}.items():
        assert actualenv[k] == v
    assert app["Image"] == ALL["environment"]["image_id"]
    assert app["State"]["Status"] == "running"
    assert all(not x["RW"] for x in app["Mounts"] if x["Type"] == "bind")
    assert docker("exec", app["Id"], "cat", "/fixture/data.txt").stdout == "native fixture bytes\n"
    assert hashes(MAIN) == before
    return {"source_unchanged": True, "container_ids": [x["Id"] for x in snapshot["container"]], "networks": [x["Name"] for x in snapshot["network"]], "volumes": [x["Name"] for x in snapshot["volume"]], "image_id": app["Image"], "app_environment_assertions": 8}

def inspect_main():
    snapshot = resources("main")
    records = []
    for kind, entries in snapshot.items():
        for r in entries:
            labels = r["Config"]["Labels"] if kind == "container" else r["Labels"]
            assert labels["com.docker.compose.project"] == project("main")
            if kind in ("container", "network"):
                assert len(r["Id"]) == 64 and all(c in "0123456789abcdef" for c in r["Id"])
            item = {"kind":kind,"name":r["Name"],"labels":labels}
            if kind == "container":
                assert "Health" not in r["State"]
                assert labels["com.docker.compose.container-number"] == "1"
                item |= {"id":r["Id"],"created":r["Created"],"health_field_present":False,"state":r["State"]["Status"]}
            if kind == "volume":
                assert r["Driver"] == "local" and r["CreatedAt"]
                item["weak_fingerprint"] = r["Name"] + "|" + r["Driver"] + "|" + r["CreatedAt"]
            records.append(item)
    app = next(x for x in snapshot["container"] if x["Config"]["Labels"]["com.docker.compose.service"] == "app")
    docker("container", "stop", "--time", "1", app["Id"])
    assert app["Id"] in ids("main")
    assert jdocker("container", "inspect", app["Id"])[0]["State"]["Status"] == "exited"
    docker("container", "start", app["Id"])
    return {"records": records, "all_list_contains_stopped": True, "stack_association": "No product ledger involved; labels do not establish stack_id."}

def no_build():
    p = fixture("build", {"compose.yaml": "services:\n  app:\n    image: ${NC_IMAGE}\n    build: ./buildctx\n    command: [sh, -c, 'sleep 300']\n", "buildctx/Dockerfile": "FROM " + ALL["environment"]["image_ref"] + "\nLABEL dmnc.build_marker=" + NS + "\n"})
    marker_args = ("image", "ls", "--filter", "label=dmnc.build_marker=" + NS, "--quiet")
    assert docker(*marker_args).stdout.strip() == ""
    model(p, "build")
    compose(p, "build", "up", "--detach", "--no-build")
    r = resources("build")["container"][0]
    assert r["Image"] == ALL["environment"]["image_id"]
    assert docker(*marker_args).stdout.strip() == ""
    missing = fixture("noimage", {"compose.yaml":"services:\n  app:\n    build: ../build/buildctx\n    command: [true]\n"})
    result = compose(missing, "noimage", "up", "--detach", "--no-build", ok=None)
    assert result.returncode != 0
    assert docker(*marker_args).stdout.strip() == ""
    return {"actual_image_id":r["Image"], "build_marker_images":[], "no_image_exit":result.returncode, "no_image_resource_ids":ids("noimage")}

def external():
    n, v = NS + "_external_net", NS + "_external_vol"
    p = fixture("external", {"compose.yaml":"services:\n  app:\n    image: ${NC_IMAGE}\n    command: [sh, -c, 'sleep 300']\n    networks: [outside]\n    volumes: [outside:/state]\nnetworks:\n  outside:\n    external: true\n    name: " + n + "\nvolumes:\n  outside:\n    external: true\n    name: " + v + "\n"})
    model(p, "external")
    result = compose(p, "external", "up", "--detach", "--no-build", ok=None)
    assert result.returncode != 0
    assert docker("network", "inspect", n, ok=None).returncode != 0
    assert docker("volume", "inspect", v, ok=None).returncode != 0
    docker("network", "create", "--label", "dmnc.test.owner=" + NS, n)
    docker("volume", "create", "--label", "dmnc.test.owner=" + NS, v)
    compose(p, "external", "up", "--detach", "--no-build")
    assert len(ids("external")) == 1 and ids("external", "network") == [] and ids("external", "volume") == []
    ni, vi = jdocker("network", "inspect", n)[0], jdocker("volume", "inspect", v)[0]
    assert "com.docker.compose.project" not in ni["Labels"]
    assert "com.docker.compose.project" not in vi["Labels"]
    return {"missing_exit":result.returncode, "managed_external_networks":[], "managed_external_volumes":[], "network_labels":ni["Labels"], "volume_labels":vi["Labels"]}

def collision():
    name = NS + "_foreign_name"
    foreign = docker("run", "--detach", "--name", name, "--label", "dmnc.test.owner=" + NS, ALL["environment"]["image_ref"], "sh", "-c", "sleep 300").stdout.strip()
    original = jdocker("container", "inspect", foreign)[0]
    p = fixture("collision", {"compose.yaml":"services:\n  app:\n    image: ${NC_IMAGE}\n    container_name: " + name + "\n    command: [sh, -c, 'sleep 300']\n"})
    model(p, "collision")
    result = compose(p, "collision", "up", "--detach", "--no-build", ok=None)
    assert result.returncode != 0
    after = jdocker("container", "inspect", foreign)[0]
    assert after["Id"] == original["Id"] and after["State"]["Status"] == "running"
    assert "com.docker.compose.project" not in after["Config"]["Labels"]
    return {"exit":result.returncode, "original_id_preserved":foreign, "project_container_ids":ids("collision"), "project_network_ids_after_error":ids("collision", "network")}

def replace_orphan():
    before = resources("main")
    old = next(x for x in before["container"] if x["Config"]["Labels"]["com.docker.compose.service"] == "app")
    dormant = next(x for x in before["container"] if x["Config"]["Labels"]["com.docker.compose.service"] == "dormant")
    override = MAIN / "sub/override.yaml"
    override.write_text(override.read_text() + "    command: [sh, -c, 'sleep 301']\n")
    compose(MAIN, "main", "up", "--detach", "--no-build", **MAIN_KW)
    new = next(x for x in resources("main")["container"] if x["Config"]["Labels"]["com.docker.compose.service"] == "app")
    assert new["Id"] != old["Id"]
    assert new["Config"]["Labels"]["com.docker.compose.container-number"] == old["Config"]["Labels"]["com.docker.compose.container-number"]
    source = MAIN / "compose.yaml"
    text = source.read_text()
    source.write_text(text[:text.index("  dormant:")] + "volumes:\n  data: {}\n")
    kwargs = MAIN_KW | {"profiles":()}
    result = compose(MAIN, "main", "up", "--detach", "--no-build", **kwargs)
    assert dormant["Id"] in ids("main")
    return {"old_app_id":old["Id"],"new_app_id":new["Id"],"logical_key":"app/1", "orphan_id_preserved":dormant["Id"],"orphan_warning_observed":"orphan" in result.stderr.lower()}

def partial_failure():
    p = fixture("partial", {"compose.yaml":"""services:
  good:
    image: ${NC_IMAGE}
    command: [sh, -c, 'sleep 300']
  bad:
    image: ${NC_IMAGE}
    depends_on:
      good:
        condition: service_started
    entrypoint: [/dmnc-nonexistent-executable]
"""})
    result = compose(p, "partial", "up", "--detach", "--no-build", ok=None)
    assert result.returncode != 0
    snapshot = resources("partial")
    good = next(x for x in snapshot["container"] if x["Config"]["Labels"]["com.docker.compose.service"] == "good")
    assert good["State"]["Status"] == "running"
    return {"exit":result.returncode,"states":{x["Config"]["Labels"]["com.docker.compose.service"]:x["State"]["Status"] for x in snapshot["container"]},"network_count":len(snapshot["network"])}

def cancel_waiting_up():
    p = fixture("cancel", {"compose.yaml":"""services:
  slow:
    image: ${NC_IMAGE}
    command: [sh, -c, 'sleep 300']
    healthcheck:
      test: [CMD, 'false']
      interval: 1s
      timeout: 1s
      retries: 200
      start_period: 200s
  after:
    image: ${NC_IMAGE}
    command: [sh, -c, 'sleep 300']
    depends_on:
      slow:
        condition: service_healthy
"""})
    argv = compose_argv(p, "cancel", ["up", "--detach", "--no-build"])
    env = BASE_ENV | {"NC_IMAGE":ALL["environment"]["image_ref"], "FIXTURE_TOKEN":TOKEN}
    started = stamp()
    proc = subprocess.Popen(argv, cwd=p, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
    found_running = False
    try:
        until = time.monotonic() + 20
        while time.monotonic() < until and proc.poll() is None:
            raw = docker("container", "ls", "--all", "--no-trunc", "--filter", "label=com.docker.compose.project=" + project("cancel"), "--format", "{{.ID}} {{.State}} {{.Label \"com.docker.compose.service\"}}")
            if any("running slow" in line for line in raw.stdout.splitlines()):
                found_running = True
                break
            time.sleep(0.5)
        assert found_running and proc.poll() is None, "never reached cancellable healthy-wait boundary"
        os.killpg(proc.pid, signal.SIGTERM)
        try:
            stdout, stderr = proc.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            stdout, stderr = proc.communicate(timeout=5)
        log({"started":started,"argv":argv,"cwd":str(p),"env_keys":sorted(env),"injected_signal":"SIGTERM process group","exit_code":proc.returncode,"stdout":stdout,"stderr":stderr})
        assert proc.returncode != 0
        snapshot = resources("cancel")
        states = {x["Config"]["Labels"]["com.docker.compose.service"]:x["State"]["Status"] for x in snapshot["container"]}
        assert states.get("slow") == "running"
        return {"injected_at":"slow running, Compose waiting service_healthy", "exit":proc.returncode,"states_after_cancel":states,"network_count":len(snapshot["network"]),"no_rollback":True}
    finally:
        if proc.poll() is None:
            os.killpg(proc.pid, signal.SIGKILL)
            proc.communicate(timeout=5)

def cleanup():
    result = {"started":stamp(),"removed":{},"remaining":{}}
    for kind, fmt in (("container","{{.Names}}"),("network","{{.Name}}"),("volume","{{.Name}}")):
        args = [kind,"ls"] + (["--all"] if kind == "container" else []) + ["--format",fmt]
        names = [x for x in docker(*args).stdout.split() if x.startswith(NS + "_")]
        result["removed"][kind] = names
        for name in names:
            assert name.startswith(NS + "_")
            docker(kind,"rm",*(["--force"] if kind == "container" else []),name)
        result["remaining"][kind] = [x for x in docker(*args).stdout.split() if x.startswith(NS + "_")]
        assert result["remaining"][kind] == []
    result["finished"] = stamp()
    ALL["cleanup"] = result
    return result

try:
    case("NC01", setup)
    if "image_ref" in ALL["environment"]:
        for n, fn in (("NC02",config_cases),("NC03",apply_main),("NC04",inspect_main),("NC05",no_build),("NC06",external),("NC07",collision),("NC08",replace_orphan),("NC09",partial_failure),("NC10",cancel_waiting_up)):
            case(n,fn)
finally:
    case("NC11",cleanup)
    ALL["finished"] = stamp()
    save()
    print(json.dumps({"results":str(RESULT),"log":str(LOG),"counts":{s:sum(x["status"]==s for x in ALL["cases"]) for s in ("PASS","FAIL")}}),flush=True)
