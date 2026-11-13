# A Web and CLI pre-execution baseline
Saved 2026-11-13 before this agent starts commands on A. Only m_agq5jab6pmtal66qwxntxjkvyq is allowed.

WC01: Existing run-web-checks.sh performs npm ci, all 39 Web tests, typecheck and build once, records full stdout/stderr/exit codes; no package/lock/source fixes.
WC02: Official built manager init/serve provides built Web index and local assets over loopback TLS; an explicit private CA is trusted by clients. Manager init uses controlled stdin and private generated test password.
WC03: Actual HTTPS login, stable session, explicit grants, node creation and one-time token claim; secrets are never printed or downloaded.
WC04: Official built agent init/enroll/serve uses its own private config and identity, real local Docker CLI/socket, TLS verification; empty production runtime allowlist yields empty capabilities.
WC05: Actual WS empty recovery reaches unfrozen completed, zero rows; no fixture/API injection. If public production Store readers are needed, use a read-only observer with existing exact keys and no Create/Open side effects.
WC06: Real multi-file config save and reread preserves original content; Apply while online with empty capabilities returns 422 unsupported and creates no operation.
WC07: Trap stops only owned service PIDs, waits for exits, checks no owned supervisor and reacquires both runtime locks; no Docker daemon action, no external listener.
WC08: Report component automation/real executable+HTTP separately from browser UI; browser UI not executed by this task. Production Docker deployment remains unverified and unavailable.

Evidence under /data/docker-manager-debug/evidence/web-first and /data/docker-manager-debug/smoke; local report .cache/m1-instabox-web-cli-report.md.

