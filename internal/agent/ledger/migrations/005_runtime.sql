PRAGMA user_version = 5;

CREATE TABLE release_control (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    quarantined INTEGER NOT NULL DEFAULT 0 CHECK (quarantined IN (0, 1))
) STRICT;

CREATE TABLE finalizations (
    operation_id TEXT PRIMARY KEY NOT NULL REFERENCES executions(operation_id) ON DELETE RESTRICT,
    result_event_seq INTEGER NOT NULL CHECK (result_event_seq BETWEEN 1 AND 9007199254740991),
    terminal_state TEXT NOT NULL CHECK (terminal_state IN ('succeeded', 'failed', 'canceled')),
    body_cipher BLOB NOT NULL CHECK (length(body_cipher) >= 28),
    wrapped_dek BLOB NOT NULL CHECK (length(wrapped_dek) = 60),
    key_id TEXT NOT NULL REFERENCES event_keys(key_id) ON DELETE RESTRICT,
    body_fingerprint BLOB NOT NULL CHECK (length(body_fingerprint) = 32),
    FOREIGN KEY (operation_id, result_event_seq) REFERENCES execution_events(operation_id, event_seq) ON DELETE RESTRICT
) STRICT;

CREATE TABLE agent_meta (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    node_id TEXT NOT NULL,
    ledger_id TEXT NOT NULL,
    schema_version INTEGER NOT NULL CHECK (schema_version = 5),
    ledger_key_id TEXT NOT NULL CHECK (length(CAST(ledger_key_id AS BLOB)) BETWEEN 1 AND 128),
    key_check BLOB NOT NULL CHECK (length(key_check) = 32)
) STRICT;

CREATE TABLE executions (
    operation_id TEXT PRIMARY KEY NOT NULL,
    stack_id TEXT NOT NULL,
    version_id TEXT NOT NULL,
    input_digest BLOB NOT NULL CHECK (length(input_digest) = 32),
    local_fingerprint BLOB NOT NULL CHECK (length(local_fingerprint) = 32),
    project_name TEXT NOT NULL,
    docker_endpoint_id TEXT NOT NULL,
    policy_revision INTEGER NOT NULL CHECK (policy_revision BETWEEN 0 AND 9007199254740991),
    prepared_session TEXT NOT NULL,
    owner_boot_id TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (phase IN ('preparing', 'ready', 'START_COMMITTED')),
    session_id TEXT,
    start_nonce TEXT,
    permit_id TEXT,
    auth_rev INTEGER CHECK (auth_rev BETWEEN 0 AND 9007199254740991),
    reply_to TEXT,
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    CHECK (
        (phase IN ('preparing', 'ready') AND session_id IS NULL AND start_nonce IS NULL AND permit_id IS NULL AND auth_rev IS NULL AND reply_to IS NULL AND started_at IS NULL)
        OR
        (phase = 'START_COMMITTED' AND session_id IS NOT NULL AND start_nonce IS NOT NULL AND permit_id IS NOT NULL AND auth_rev IS NOT NULL AND reply_to IS NOT NULL AND started_at IS NOT NULL)
    )
) STRICT;

CREATE TABLE local_claims (
    resource_kind TEXT NOT NULL CHECK (resource_kind IN ('stack', 'project')),
    resource_id TEXT NOT NULL,
    operation_id TEXT NOT NULL REFERENCES executions(operation_id) ON DELETE RESTRICT,
    PRIMARY KEY (resource_kind, resource_id)
) STRICT;

CREATE INDEX local_claims_operation ON local_claims(operation_id);

CREATE TABLE event_keys (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    key_id TEXT NOT NULL UNIQUE CHECK (length(CAST(key_id AS BLOB)) BETWEEN 1 AND 128),
    key_check BLOB NOT NULL CHECK (length(key_check) = 32),
    uses INTEGER NOT NULL CHECK (uses BETWEEN 0 AND 1048576)
) STRICT;

CREATE TABLE execution_events (
    operation_id TEXT NOT NULL REFERENCES executions(operation_id) ON DELETE RESTRICT,
    event_seq INTEGER NOT NULL CHECK (event_seq BETWEEN 1 AND 9007199254740991),
    kind TEXT NOT NULL CHECK (kind IN ('started', 'result')),
    body_cipher BLOB NOT NULL CHECK (length(body_cipher) >= 28),
    wrapped_dek BLOB NOT NULL CHECK (length(wrapped_dek) = 60),
    key_id TEXT NOT NULL REFERENCES event_keys(key_id) ON DELETE RESTRICT,
    body_fingerprint BLOB NOT NULL CHECK (length(body_fingerprint) = 32),
    ack_session TEXT,
    PRIMARY KEY (operation_id, event_seq)
) STRICT;

CREATE TABLE event_state (
    operation_id TEXT PRIMARY KEY NOT NULL REFERENCES executions(operation_id) ON DELETE RESTRICT,
    last_seq INTEGER NOT NULL CHECK (last_seq BETWEEN 0 AND 9007199254740991),
    latest_result_seq INTEGER CHECK (latest_result_seq BETWEEN 1 AND last_seq),
    blocked INTEGER NOT NULL CHECK (blocked IN (0, 1)),
    FOREIGN KEY (operation_id, latest_result_seq) REFERENCES execution_events(operation_id, event_seq) ON DELETE RESTRICT
) STRICT;

CREATE TABLE event_conflicts (
    operation_id TEXT NOT NULL REFERENCES executions(operation_id) ON DELETE RESTRICT,
    event_seq INTEGER NOT NULL CHECK (event_seq BETWEEN 1 AND 9007199254740991),
    body_fingerprint BLOB NOT NULL CHECK (length(body_fingerprint) = 32),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (operation_id, event_seq, body_fingerprint)
) STRICT;

CREATE TABLE recovery_control (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    active_boot TEXT NOT NULL,
    current_session TEXT NOT NULL DEFAULT '',
    frozen INTEGER NOT NULL DEFAULT 1 CHECK (frozen IN (0, 1)),
    active_scan_id TEXT NOT NULL DEFAULT '',
    active_scope TEXT NOT NULL DEFAULT '',
    active_limit INTEGER NOT NULL DEFAULT 0,
    active_total INTEGER NOT NULL DEFAULT 0 CHECK (active_total BETWEEN 0 AND 9007199254740991),
    dirty INTEGER NOT NULL DEFAULT 0 CHECK (dirty IN (0, 1)),
    completed_scan_id TEXT NOT NULL DEFAULT '',
    completed_scope TEXT NOT NULL DEFAULT '',
    completed_total INTEGER NOT NULL DEFAULT 0 CHECK (completed_total BETWEEN 0 AND 9007199254740991),
    CHECK ((active_scan_id = '' AND active_scope = '' AND active_limit = 0 AND active_total = 0)
        OR (active_scan_id <> '' AND active_scope IN ('pending', 'all') AND active_limit BETWEEN 1 AND 200 AND frozen = 1)),
    CHECK ((completed_scan_id = '' AND completed_scope = '' AND completed_total = 0)
        OR (completed_scan_id <> '' AND completed_scope IN ('pending', 'all')))
) STRICT;

CREATE TABLE recovery_members (
    scan_id TEXT NOT NULL CHECK (scan_id <> ''),
    ordinal INTEGER NOT NULL CHECK (ordinal BETWEEN 0 AND 9007199254740991),
    operation_id TEXT NOT NULL REFERENCES executions(operation_id) ON DELETE RESTRICT,
    last_event_seq INTEGER NOT NULL CHECK (last_event_seq BETWEEN 0 AND 9007199254740991),
    result_event_seq INTEGER NOT NULL CHECK (result_event_seq BETWEEN 0 AND last_event_seq),
    phase TEXT NOT NULL CHECK (phase IN ('preparing', 'ready', 'START_COMMITTED', 'result', 'reconciliation_required', 'finalized')),
    report_acked INTEGER NOT NULL CHECK (report_acked IN (0, 1)),
    record_fingerprint BLOB NOT NULL CHECK (length(record_fingerprint) = 32),
    PRIMARY KEY (scan_id, ordinal),
    UNIQUE (scan_id, operation_id),
    CHECK (result_event_seq <> 0 OR report_acked = 0)
) STRICT;

CREATE TABLE recovery_changes (
    operation_id TEXT NOT NULL,
    event_seq INTEGER NOT NULL CHECK (event_seq BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (operation_id, event_seq),
    FOREIGN KEY (operation_id, event_seq) REFERENCES execution_events(operation_id, event_seq) ON DELETE RESTRICT
) STRICT;


CREATE TABLE runtime_profile (
 id INTEGER PRIMARY KEY CHECK (id=1), endpoint_id TEXT NOT NULL,
 policy_revision INTEGER NOT NULL CHECK (policy_revision BETWEEN 0 AND 9007199254740991),
 fingerprint TEXT NOT NULL, engine_id TEXT NOT NULL, docker_version TEXT NOT NULL, compose_version TEXT NOT NULL
) STRICT;
CREATE TABLE runtime_documents (
 operation_id TEXT NOT NULL REFERENCES executions(operation_id) ON DELETE RESTRICT,
 kind TEXT NOT NULL CHECK (kind IN ('input','preparation','process','candidate')),
 seq INTEGER NOT NULL CHECK(seq BETWEEN 0 AND 9007199254740991),
 body_cipher BLOB NOT NULL CHECK(length(body_cipher)>=28), wrapped_dek BLOB NOT NULL CHECK(length(wrapped_dek)=60),
 key_id TEXT NOT NULL REFERENCES event_keys(key_id) ON DELETE RESTRICT,
 body_fingerprint BLOB NOT NULL CHECK(length(body_fingerprint)=32), PRIMARY KEY(operation_id,kind,seq)
) STRICT;
CREATE TABLE process_facts (
 operation_id TEXT PRIMARY KEY NOT NULL REFERENCES executions(operation_id) ON DELETE RESTRICT,
 owner_boot TEXT NOT NULL, spawn_state TEXT NOT NULL CHECK(spawn_state IN ('not_attempted','created','exited','not_started')),
 process_id INTEGER, cancel_reason TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE TABLE project_bindings (
 endpoint_id TEXT NOT NULL, project_name TEXT NOT NULL, stack_id TEXT NOT NULL,
 ownership_revision INTEGER NOT NULL CHECK(ownership_revision BETWEEN 0 AND 9007199254740991),
 status TEXT NOT NULL CHECK(status IN ('provisional','active','drifted')),
 last_operation_id TEXT REFERENCES executions(operation_id) ON DELETE RESTRICT, baseline_seq INTEGER,
 PRIMARY KEY(endpoint_id,project_name),
 CHECK((last_operation_id IS NULL AND baseline_seq IS NULL) OR (last_operation_id IS NOT NULL AND baseline_seq IS NOT NULL))
) STRICT;
CREATE TABLE observation_queries (
 query_id TEXT PRIMARY KEY NOT NULL, operation_id TEXT NOT NULL REFERENCES executions(operation_id) ON DELETE RESTRICT,
 owner_boot TEXT NOT NULL, page_limit INTEGER NOT NULL CHECK(page_limit BETWEEN 1 AND 200), created_at INTEGER NOT NULL,
 result_seq INTEGER, FOREIGN KEY(operation_id,result_seq) REFERENCES execution_events(operation_id,event_seq) ON DELETE RESTRICT
) STRICT;
