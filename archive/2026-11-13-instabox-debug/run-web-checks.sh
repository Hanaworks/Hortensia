#!/usr/bin/env bash
set -u
umask 077
export PATH=/data/docker-manager-debug/tools/node-v24.15.0-linux-x64/bin:$PATH
export npm_config_cache=/data/docker-manager-debug/tools/npm-cache
cd /data/docker-manager-debug/source/web || exit 2
run_dir=/data/docker-manager-debug/evidence/web-first
if test -e "$run_dir"; then exit 2; fi
mkdir "$run_dir" || exit 2
date -u +%Y-%m-%dT%H:%M:%SZ > "$run_dir/started.txt"
{ node --version; npm --version; } > "$run_dir/environment.txt" 2>&1
printf '%s\n' running > "$run_dir/status"
overall=0
check() {
  local name="$1"
  shift
  "$@" > "$run_dir/$name.stdout" 2> "$run_dir/$name.stderr"
  local rc=$?
  printf '%s\n' "$rc" > "$run_dir/$name.exit"
  if test "$rc" -ne 0; then overall=1; fi
  return "$rc"
}
if check npm-ci npm ci; then
  check npm-test npm test
  check npm-typecheck npm run typecheck
  check npm-build npm run build
fi
date -u +%Y-%m-%dT%H:%M:%SZ > "$run_dir/finished.txt"
printf '%s\n' "$overall" > "$run_dir/overall.exit"
printf '%s\n' finished > "$run_dir/status"
exit "$overall"
