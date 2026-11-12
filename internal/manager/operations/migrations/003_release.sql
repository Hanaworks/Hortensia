PRAGMA user_version = 3;

CREATE TABLE manager_meta (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    schema_version INTEGER NOT NULL CHECK (schema_version = 3),
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
    release_status TEXT NOT NULL CHECK (release_status IN ('held', 'pending', 'released')),
    blocked INTEGER NOT NULL CHECK (blocked IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    CHECK (latest_result_seq <= max_event_seq),
    CHECK ((permit_id = '' AND permit_session = '' AND start_nonce = '' AND auth_rev = 0) OR (permit_id <> '' AND permit_session <> '' AND start_nonce <> '' AND auth_rev > 0)),
    CHECK (release_status = 'held' OR (release_status='pending' AND latest_result_seq > 0 AND state IN ('succeeded', 'failed', 'canceled') AND blocked = 0) OR (release_status='released' AND blocked=0 AND ((latest_result_seq>0 AND state IN ('succeeded','failed','canceled')) OR (state='canceled' AND cancel_requested=1 AND permit_id='' AND max_event_seq=0))))
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

CREATE TABLE recovery_nodes (
 node_id TEXT PRIMARY KEY NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
 agent_boot TEXT NOT NULL,
 quarantined INTEGER NOT NULL DEFAULT 0 CHECK(quarantined IN (0,1)),
 active_round TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE recovery_scans (
 id TEXT PRIMARY KEY NOT NULL,
 node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
 ledger_id TEXT NOT NULL,
 session_id TEXT NOT NULL,
 manager_boot TEXT NOT NULL,
 agent_boot TEXT NOT NULL,
 scope TEXT NOT NULL CHECK(scope IN ('pending','all')),
 page_limit INTEGER NOT NULL CHECK(page_limit BETWEEN 1 AND 200),
 scan_id TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL CHECK(status IN ('collecting','enumerated','need_all','waiting','blocked','ready','completing','completed','abandoned','expired')),
 total INTEGER NOT NULL DEFAULT -1 CHECK(total BETWEEN -1 AND 4096),
 seen INTEGER NOT NULL DEFAULT 0 CHECK(seen BETWEEN 0 AND 4096),
 pages INTEGER NOT NULL DEFAULT 0 CHECK(pages BETWEEN 0 AND 4096),
 body_bytes INTEGER NOT NULL DEFAULT 0 CHECK(body_bytes BETWEEN 0 AND 33554432),
 next_cursor TEXT NOT NULL DEFAULT '',
 last_operation TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE recovery_requests (
 message_id TEXT PRIMARY KEY NOT NULL,
 round_id TEXT NOT NULL REFERENCES recovery_scans(id) ON DELETE RESTRICT,
 kind TEXT NOT NULL CHECK(kind IN ('reconcile','reconcile_complete')),
 page_no INTEGER NOT NULL,
 cursor TEXT NOT NULL,
 raw BLOB NOT NULL CHECK(length(raw) BETWEEN 1 AND 8388608),
 accepted INTEGER NOT NULL CHECK(accepted IN (0,1)),
 UNIQUE(round_id,kind,page_no),
 CHECK((kind='reconcile' AND page_no BETWEEN 0 AND 4095) OR (kind='reconcile_complete' AND page_no=-1))
) STRICT;

CREATE TABLE recovery_pages (
 round_id TEXT NOT NULL REFERENCES recovery_scans(id) ON DELETE RESTRICT,
 page_no INTEGER NOT NULL CHECK(page_no BETWEEN 0 AND 4095),
 request_id TEXT NOT NULL UNIQUE REFERENCES recovery_requests(message_id) ON DELETE RESTRICT,
 body_cipher BLOB NOT NULL CHECK(length(body_cipher) BETWEEN 28 AND 8388636),
 wrapped_dek BLOB NOT NULL CHECK(length(wrapped_dek)=60),
 key_id TEXT NOT NULL REFERENCES keys(encryption_key_id) ON DELETE RESTRICT,
 fingerprint BLOB NOT NULL CHECK(length(fingerprint)=32),
 PRIMARY KEY(round_id,page_no)
) STRICT;

CREATE TABLE recovery_members (
 round_id TEXT NOT NULL REFERENCES recovery_scans(id) ON DELETE RESTRICT,
 ordinal INTEGER NOT NULL CHECK(ordinal BETWEEN 0 AND 4095),
 operation_id TEXT NOT NULL,
 phase TEXT NOT NULL CHECK(phase IN ('preparing','ready','START_COMMITTED','result','reconciliation_required','finalized')),
 page_no INTEGER NOT NULL,
 record_no INTEGER NOT NULL CHECK(record_no BETWEEN 0 AND 199),
 PRIMARY KEY(round_id,ordinal),
 UNIQUE(round_id,operation_id),
 FOREIGN KEY(round_id,page_no) REFERENCES recovery_pages(round_id,page_no) ON DELETE RESTRICT
) STRICT;

CREATE INDEX recovery_members_operation_phase ON recovery_members(operation_id,phase,round_id);

CREATE TABLE recovery_cases (
 id TEXT PRIMARY KEY NOT NULL,
 node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
 ledger_id TEXT NOT NULL,
 operation_id TEXT NOT NULL,
 kind TEXT NOT NULL,
 round_id TEXT NOT NULL,
 evidence_cipher BLOB NOT NULL CHECK(length(evidence_cipher) BETWEEN 28 AND 8388636),
 wrapped_dek BLOB NOT NULL CHECK(length(wrapped_dek)=60),
 key_id TEXT NOT NULL REFERENCES keys(encryption_key_id) ON DELETE RESTRICT,
 fingerprint BLOB NOT NULL CHECK(length(fingerprint)=32),
 UNIQUE(node_id,operation_id,kind,fingerprint)
) STRICT;

CREATE TABLE absence_releases (
 operation_id TEXT PRIMARY KEY NOT NULL REFERENCES operations(operation_id) ON DELETE RESTRICT,
 round_id TEXT NOT NULL REFERENCES recovery_scans(id) ON DELETE RESTRICT
) STRICT;

CREATE TABLE recovery_expected (
 round_id TEXT NOT NULL REFERENCES recovery_scans(id) ON DELETE RESTRICT,
 operation_id TEXT NOT NULL REFERENCES operations(operation_id) ON DELETE RESTRICT,
 last_event_seq INTEGER NOT NULL CHECK(last_event_seq BETWEEN 0 AND 2),
 PRIMARY KEY(round_id,operation_id)
) STRICT;

CREATE TABLE finalize_requests (
 message_id TEXT PRIMARY KEY NOT NULL,
 operation_id TEXT NOT NULL REFERENCES operations(operation_id) ON DELETE RESTRICT,
 node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
 ledger_id TEXT NOT NULL,
 session_id TEXT NOT NULL,
 manager_boot TEXT NOT NULL,
 agent_boot TEXT NOT NULL,
 round_id TEXT NOT NULL,
 raw BLOB NOT NULL CHECK(length(raw) BETWEEN 1 AND 8388608),
 accepted INTEGER NOT NULL CHECK(accepted IN (0,1)),
 confirmation_mac BLOB NOT NULL DEFAULT X'' CHECK(length(confirmation_mac) IN (0,32)),
 UNIQUE(operation_id,node_id,ledger_id,session_id,manager_boot,agent_boot,round_id)
) STRICT;

CREATE TABLE finalization (
 operation_id TEXT PRIMARY KEY NOT NULL REFERENCES operations(operation_id) ON DELETE RESTRICT,
 request_id TEXT NOT NULL UNIQUE REFERENCES finalize_requests(message_id) ON DELETE RESTRICT,
 result_event_seq INTEGER NOT NULL CHECK(result_event_seq BETWEEN 1 AND 2),
 terminal_state TEXT NOT NULL CHECK(terminal_state IN ('succeeded','failed','canceled')),
 body_cipher BLOB NOT NULL CHECK(length(body_cipher) BETWEEN 28 AND 8388636),
 wrapped_dek BLOB NOT NULL CHECK(length(wrapped_dek)=60),
 key_id TEXT NOT NULL REFERENCES keys(encryption_key_id) ON DELETE RESTRICT,
 fingerprint BLOB NOT NULL CHECK(length(fingerprint)=32)
) STRICT;

