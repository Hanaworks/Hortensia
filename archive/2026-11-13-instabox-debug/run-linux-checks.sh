#!/usr/bin/env bash
set -u
umask 077
cd /data/docker-manager-debug/source || exit 2
run_dir=/data/docker-manager-debug/evidence/linux-first
if test -e "$run_dir"; then
  printf '%s\n' 'Evidence destination already exists; retain previous results.' >&2
  exit 2
fi
mkdir -p "$run_dir" || exit 2
date -u +%Y-%m-%dT%H:%M:%SZ > "$run_dir/started.txt"
{
  uname -a
  cat /etc/os-release
  id
  ps -p 1 -o comm=
  nproc
  free -m
  df -h / /data
  go version
  go env GOOS GOARCH CGO_ENABLED GOTOOLCHAIN
  node --version
  npm --version
  docker version --format '{{json .}}'
  docker compose version
  docker info --format '{{json .ID}} {{json .Driver}} {{json .CgroupVersion}} {{json .LiveRestoreEnabled}}'
} > "$run_dir/environment.txt" 2>&1
python3 - /data/docker-manager-debug/source-manifest.json <<'PY' > "$run_dir/input-verification.txt" 2>&1
import hashlib, json, pathlib, sys
root=pathlib.Path.cwd().resolve()
manifest=json.loads(pathlib.Path(sys.argv[1]).read_text())
for entry in manifest['files']:
    p=(root/entry['path']).resolve()
    if root not in p.parents or not p.is_file() or p.is_symlink():
        raise SystemExit('Invalid source path')
    raw=p.read_bytes()
    if len(raw)!=entry['size'] or hashlib.sha256(raw).hexdigest()!=entry['sha256']:
        raise SystemExit('Source mismatch: '+entry['path'])
print(json.dumps({'head':manifest['head'],'files':len(manifest['files']),'verified':True}))
PY
input_status=$?
printf '%s\n' "$input_status" > "$run_dir/input-verification.exit"
if test "$input_status" -ne 0; then
  printf '%s\n' 'input_failed' > "$run_dir/status"
  exit "$input_status"
fi
printf '%s\n' 'running' > "$run_dir/status"
overall=0
check() {
  local name="$1"
  shift
  "$@" > "$run_dir/$name.stdout" 2> "$run_dir/$name.stderr"
  local rc=$?
  printf '%s\n' "$rc" > "$run_dir/$name.exit"
  if test "$rc" -ne 0; then overall=1; fi
}
check go-test go test -p 1 ./... -count=1 -json
check go-vet go vet ./...
check go-build go build ./...
check go-mod-verify go mod verify
date -u +%Y-%m-%dT%H:%M:%SZ > "$run_dir/finished.txt"
printf '%s\n' "$overall" > "$run_dir/overall.exit"
printf '%s\n' 'finished' > "$run_dir/status"
exit "$overall"
