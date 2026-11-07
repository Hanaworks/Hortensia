PRAGMA user_version = 1;

CREATE TABLE manager_meta (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    schema_version INTEGER NOT NULL CHECK (schema_version = 1),
    manager_id TEXT NOT NULL,
    active_boot TEXT NOT NULL,
    recovery_mode INTEGER NOT NULL CHECK (recovery_mode IN (0, 1))
) STRICT;

CREATE TABLE keys (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    digest_key_id TEXT NOT NULL UNIQUE CHECK (length(CAST(digest_key_id AS BLOB)) BETWEEN 1 AND 128),
    encryption_key_id TEXT NOT NULL UNIQUE CHECK (length(CAST(encryption_key_id AS BLOB)) BETWEEN 1 AND 128),
    digest_key_check BLOB NOT NULL CHECK (length(digest_key_check) = 32),
    encryption_key_check BLOB NOT NULL CHECK (length(encryption_key_check) = 32),
    uses INTEGER NOT NULL CHECK (uses BETWEEN 0 AND 1048576),
    CHECK (digest_key_id <> encryption_key_id)
) STRICT;

CREATE TABLE users (
    id TEXT PRIMARY KEY NOT NULL,
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    auth_rev INTEGER NOT NULL CHECK (auth_rev BETWEEN 1 AND 9007199254740991)
) STRICT;

CREATE TABLE nodes (
    id TEXT PRIMARY KEY NOT NULL,
    ledger_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    boot_id TEXT NOT NULL,
    frozen INTEGER NOT NULL CHECK (frozen IN (0, 1))
) STRICT;

CREATE TABLE grants (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    capability TEXT NOT NULL CHECK (capability IN ('node.read', 'config.read', 'config.write', 'stack.deploy', 'operation.read', 'operation.output.read', 'operation.cancel')),
    PRIMARY KEY (user_id, node_id, capability)
) STRICT;

CREATE TABLE operations (
    operation_id TEXT PRIMARY KEY NOT NULL,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    ledger_id TEXT NOT NULL,
    stack_id TEXT NOT NULL,
    version_id TEXT NOT NULL,
    project_name TEXT NOT NULL,
    endpoint_id TEXT NOT NULL,
    policy_revision INTEGER NOT NULL CHECK (policy_revision BETWEEN 0 AND 9007199254740991),
    input_digest BLOB NOT NULL CHECK (length(input_digest) = 32),
    owner_boot TEXT NOT NULL,
    prepared_session TEXT NOT NULL,
    payload_cipher BLOB NOT NULL CHECK (length(payload_cipher) BETWEEN 28 AND 8388636),
    wrapped_dek BLOB NOT NULL CHECK (length(wrapped_dek) = 60),
    key_id TEXT NOT NULL REFERENCES keys(encryption_key_id) ON DELETE RESTRICT,
    fingerprint BLOB NOT NULL CHECK (length(fingerprint) = 32),
    state TEXT NOT NULL CHECK (state IN ('accepted', 'ready', 'authorized', 'running', 'succeeded', 'failed', 'canceled', 'reconciliation_required')),
    revision INTEGER NOT NULL CHECK (revision BETWEEN 1 AND 9007199254740991),
    cancel_requested INTEGER NOT NULL CHECK (cancel_requested IN (0, 1)),
    permit_id TEXT NOT NULL DEFAULT '',
    permit_session TEXT NOT NULL DEFAULT '',
    start_nonce TEXT NOT NULL DEFAULT '',
    auth_rev INTEGER NOT NULL DEFAULT 0 CHECK (auth_rev BETWEEN 0 AND 9007199254740991),
    latest_result_seq INTEGER NOT NULL DEFAULT 0 CHECK (latest_result_seq BETWEEN 0 AND 2),
    max_event_seq INTEGER NOT NULL DEFAULT 0 CHECK (max_event_seq BETWEEN 0 AND 2),
    release_status TEXT NOT NULL CHECK (release_status IN ('held', 'pending')),
    blocked INTEGER NOT NULL CHECK (blocked IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    CHECK (latest_result_seq <= max_event_seq),
    CHECK ((permit_id = '' AND permit_session = '' AND start_nonce = '' AND auth_rev = 0) OR (permit_id <> '' AND permit_session <> '' AND start_nonce <> '' AND auth_rev > 0)),
    CHECK (release_status = 'held' OR (latest_result_seq > 0 AND state IN ('succeeded', 'failed', 'canceled') AND blocked = 0))
) STRICT;

CREATE TABLE resource_claims (
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    resource_kind TEXT NOT NULL CHECK (resource_kind IN ('stack', 'project')),
    resource_id TEXT NOT NULL,
    operation_id TEXT NOT NULL REFERENCES operations(operation_id) ON DELETE RESTRICT,
    PRIMARY KEY (node_id, resource_kind, resource_id)
) STRICT;

CREATE INDEX resource_claims_operation ON resource_claims(operation_id);

CREATE TABLE outbox (
    operation_id TEXT NOT NULL REFERENCES operations(operation_id) ON DELETE RESTRICT,
    kind TEXT NOT NULL CHECK (kind IN ('prepare', 'cancel', 'finalize')),
    generation INTEGER NOT NULL CHECK (generation = 1),
    delivery_state TEXT NOT NULL CHECK (delivery_state IN ('pending', 'confirmed', 'suppressed')),
    result_event_seq INTEGER NOT NULL DEFAULT 0 CHECK (result_event_seq BETWEEN 0 AND 2),
    terminal_state TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (operation_id, kind, generation),
    CHECK ((kind = 'finalize' AND result_event_seq > 0 AND terminal_state IN ('succeeded', 'failed', 'canceled')) OR (kind <> 'finalize' AND result_event_seq = 0 AND terminal_state = ''))
) STRICT;

CREATE TABLE operation_events (
    operation_id TEXT NOT NULL REFERENCES operations(operation_id) ON DELETE RESTRICT,
    source TEXT NOT NULL CHECK (source = 'agent'),
    event_seq INTEGER NOT NULL CHECK (event_seq BETWEEN 1 AND 2),
    kind TEXT NOT NULL CHECK (kind IN ('started', 'result')),
    body_cipher BLOB NOT NULL CHECK (length(body_cipher) BETWEEN 28 AND 7340060),
    wrapped_dek BLOB NOT NULL CHECK (length(wrapped_dek) = 60),
    key_id TEXT NOT NULL REFERENCES keys(encryption_key_id) ON DELETE RESTRICT,
    fingerprint BLOB NOT NULL CHECK (length(fingerprint) = 32),
    received_at INTEGER NOT NULL,
    PRIMARY KEY (operation_id, source, event_seq)
) STRICT;

CREATE TABLE event_conflicts (
    operation_id TEXT NOT NULL REFERENCES operations(operation_id) ON DELETE RESTRICT,
    event_seq INTEGER NOT NULL CHECK (event_seq BETWEEN 1 AND 2),
    kind TEXT NOT NULL CHECK (kind IN ('started', 'result')),
    fingerprint BLOB NOT NULL CHECK (length(fingerprint) = 32),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (operation_id, event_seq, kind, fingerprint)
) STRICT;
