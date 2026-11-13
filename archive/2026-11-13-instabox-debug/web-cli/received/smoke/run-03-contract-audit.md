# run-03 full pre-execution contract audit

Saved 2026-11-13 before run-03; production source frozen. Read current HTTP API sections 1-8, Manager service sections on sessions/grants, route parser, requireCaps, config mutation/query, actual Agent credentials/runtime, Manager recovery-complete persistence, Agent runtime_profile DDL/readers, Config Schema/helper and production allowlist.

| Sequence | Contract and audited expectation |
| --- | --- |
| Static GET / and assets | Official Manager web handler serves existing built dist; index 200 plus 2 referenced local assets 200. No API fixture. |
| TLS | Python3.13 strict verify, explicit new CA, SAN IP127.0.0.1, no proxy/no insecure flag; default trust rejects new CA. CA must include critical CA:TRUE and keyCertSign/cRLSign. run-01 was a fixture failure. |
| POST sessions / GET me | HTTP API sections2,4,5: exact Origin + JSON username/password, 200 Me, secure HttpOnly __Host cookie; me keeps CSRF. Password is only stdin/body in private process. |
| POST nodes | S+K; UUID key; display_name;201 node_id/status pending. |
| Pre-grant POST stacks | N+C+W+K; absent N means invisible 404/not_found under section2 and requireCaps, including admin config actions. run-02's403 was a fixture error. |
| GET user / PUT grants | S; GET200 User/ETag current auth_rev, PUT S+K+U same user auth_rev ETag, capabilities list exact supported strings,200 Grant. Initial auth_rev0 is read, not assumed1. |
| Session after grant | Manager service session rule: grant changes do not revoke sessions. Assert original GET me200/same CSRF; remove unnecessary fresh-login setup. |
| POST enrollment-tokens | S+K;{} means replace_pending false.201 first token43 chars, token remains in memory and stdin; no response body logging. |
| Agent init/enroll/serve | Official commands; init UUID; enroll --token-stdin; config absolute private paths and CA; credentials active/generation1 only inspected locally. serve uses real /usr/bin/docker unix socket. No capabilities injection. |
| Recovery | Node online is channel state, separate from capability. Actual Manager DB mode=ro/query_only reads current active round: frozen0,status completed,total0,seen0,pages>=1,matching initialized ledger. Exact production ReceiveRecoveryCompleted sets completed+unfrozen together. Public ReadRuntimeConnectionState requires live Store not exposed by CLI; opening Store would mutate boot, so do not call Open. |
| Actual Probe | HTTP ExecutionProfile exposes only endpoint/revision, not version. Agent runtime_profile mode=ro/query_only checks endpoint equals HTTP profile, engine nonempty, Docker29.8.2/Compose(v)5.6.0 from A inventory. Production allowlist remains empty and actual caps[]. |
| Multi-file create | POST stacks N+C+W+K: project regex valid; Config five unique UTF8/base64 paths, all compose/env references present, budgets far below limits. Paths include subdirs, CRLF and UTF8; save validates structure only and doesn't execute Compose.201 stack/version. Same key/input201 same original receipt with current N. |
| Version read/update | GET version N+C returns200 {version_id,config}; raw original files and ordered arrays unchanged. POST versions N+C+W+K+V {config}, If-Match quoted current UUID,201 version_id. Read old and new version separately proves immutable first version. |
| Apply | N+C+D+K; JSON version_id. Before Apply prove online/unfrozen first to distinguish503 offline/409 recovery. With caps[] required compose.apply.v1/config.bundle.v1 absent ->422 unsupported in mutateApply before operation insert. |
| Operation count | GET /operations?node_id UUID, N+O ->200 Page; before/after items[] and ro count0. Rejected Apply doesn't queue. |
| Shutdown/restart | Only created Popen objects get SIGTERM;45sec bounded wait exit0. flock while running blocks, after exit reacquires. Restart actual Manager/Agent uses persisted session/identity/same ledger; repeats current empty recovery/version reads/422+no operation. No daemon stop, no externally reachable listener. |

No production contract was changed. run-01/run-02 failures stay preserved. No browser clicking in this task;39 Web component tests and formal executable/HTTP/WS are separate evidence. This task does not prove real Compose deployment.

