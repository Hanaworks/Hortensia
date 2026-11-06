PRAGMA user_version = 2;

CREATE TABLE agent_meta (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    node_id TEXT NOT NULL,
    ledger_id TEXT NOT NULL,
    schema_version INTEGER NOT NULL CHECK (schema_version = 2),
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
    phase TEXT NOT NULL CHECK (phase IN ('ready', 'START_COMMITTED')),
    session_id TEXT,
    start_nonce TEXT,
    permit_id TEXT,
    auth_rev INTEGER CHECK (auth_rev BETWEEN 0 AND 9007199254740991),
    reply_to TEXT,
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    CHECK (
        (phase = 'ready' AND session_id IS NULL AND start_nonce IS NULL AND permit_id IS NULL AND auth_rev IS NULL AND reply_to IS NULL AND started_at IS NULL)
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
    PRIMARY KEY (operation_id, event_seq),
    UNIQUE (operation_id, kind)
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
