package ledger

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"

	"docker-manager/internal/protocol"
	"docker-manager/internal/snapshotcheck"
)

// ErrInspectionLimit means the fixed inspection budget was exceeded, not that
// a larger historical snapshot has been proven corrupt.
var ErrInspectionLimit = errors.New("ledger snapshot inspection limit")

// SnapshotInspection contains counts and an opaque keyed aggregate, never row
// plaintext or per-row secret fingerprints. It is returned only after Finish.
type SnapshotInspection struct {
	Identity                            Identity
	SchemaVersion, ProtocolVersion      int
	EncryptionKeyID                     string
	EncryptionKeyUses                   uint64
	TableRows, SealedRows               map[string]uint64
	SemanticFormat, SemanticFingerprint string
}

// InspectRuntimeSnapshot authenticates a closed private v5 single-file copy.
// It never opens a business Store or changes execution/recovery admission.
func InspectRuntimeSnapshot(ctx context.Context, path string, expected Identity, key []byte, journal JournalKey) (SnapshotInspection, error) {
	parent := ctx
	if ctx == nil {
		return SnapshotInspection{}, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return SnapshotInspection{}, err
	}
	if err := validateIdentity(expected, true); err != nil {
		return SnapshotInspection{}, err
	}
	if len(key) != 32 {
		return SnapshotInspection{}, ErrKey
	}
	if err := validateJournalKey(journal, key); err != nil {
		return SnapshotInspection{}, err
	}
	reader, err := snapshotcheck.Open(ctx, path)
	if err != nil {
		return SnapshotInspection{}, inspectionReaderError(ctx, err)
	}
	defer reader.Close()
	ctx = reader.Context()
	tx := reader.Tx()
	if err = validateSchemaQuery(ctx, tx, schemaV5); err != nil {
		return SnapshotInspection{}, inspectionContextError(ctx, err)
	}
	var version int
	if tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version) != nil {
		return SnapshotInspection{}, ErrStorage
	}
	if version != 5 {
		return SnapshotInspection{}, ErrSchema
	}
	for _, table := range inspectionTables {
		cols, e := inspectionColumns(ctx, tx, table)
		if e != nil {
			return SnapshotInspection{}, e
		}
		if e = inspectionPreflight(ctx, tx, table, cols); e != nil {
			return SnapshotInspection{}, inspectionContextError(ctx, e)
		}
	}
	for _, table := range []string{"agent_meta", "event_keys", "release_control", "recovery_control", "runtime_profile"} {
		if err = inspectionExpect(ctx, tx, 1, `SELECT count(*) FROM `+table+` WHERE id=1`); err != nil {
			return SnapshotInspection{}, err
		}
	}
	if err = inspectionIntegrity(ctx, tx); err != nil {
		return SnapshotInspection{}, inspectionContextError(ctx, err)
	}
	var actual Identity
	var check []byte
	var schema int
	if tx.QueryRowContext(ctx, `SELECT node_id,ledger_id,ledger_key_id,schema_version,key_check FROM agent_meta WHERE id=1`).Scan(&actual.NodeID, &actual.LedgerID, &actual.KeyID, &schema, &check) != nil {
		return SnapshotInspection{}, ErrSchema
	}
	if schema != 5 {
		return SnapshotInspection{}, ErrSchema
	}
	if actual != expected {
		return SnapshotInspection{}, ErrIdentity
	}
	if !hmac.Equal(check, keyCheck(actual, key)) {
		return SnapshotInspection{}, ErrKey
	}
	var keyID string
	var uses uint64
	if tx.QueryRowContext(ctx, `SELECT key_id,key_check,uses FROM event_keys WHERE id=1`).Scan(&keyID, &check, &uses) != nil {
		return SnapshotInspection{}, ErrKey
	}
	if keyID != journal.ID || !hmac.Equal(check, journalKeyCheck(actual, journal)) {
		return SnapshotInspection{}, ErrKey
	}
	if uses > uint64(maxKeyUses) {
		return SnapshotInspection{}, ErrEvidence
	}
	validator, err := protocol.NewValidator()
	if err != nil {
		return SnapshotInspection{}, ErrSchema
	}
	control, err := readScanControl(ctx, tx)
	if err != nil {
		return SnapshotInspection{}, err
	}
	s := &Store{identity: actual, key: append([]byte(nil), key...), journal: &journalConfig{keyID: journal.ID, kek: append([]byte(nil), journal.Key...)}, validator: validator, release: true, runtime: &runtimeConfig{}, boot: control.boot}
	defer s.clearOwnedKeys()
	report := SnapshotInspection{Identity: actual, SchemaVersion: 5, ProtocolVersion: 1, EncryptionKeyID: keyID, EncryptionKeyUses: uses, TableRows: map[string]uint64{}, SealedRows: map[string]uint64{"execution_events": 0, "finalizations": 0, "runtime_documents": 0}, SemanticFormat: "dm-agent-runtime-semantic-v1"}
	fp, err := snapshotcheck.NewFingerprint(key, "agent", report.SemanticFormat, []string{actual.NodeID, actual.LedgerID, actual.KeyID}, 5, 1)
	if err != nil {
		return SnapshotInspection{}, ErrKey
	}
	defer fp.Close()
	if err = s.inspectRows(ctx, tx, fp, &report); err != nil {
		return SnapshotInspection{}, inspectionContextError(ctx, err)
	}
	sealed := report.SealedRows["execution_events"] + report.SealedRows["finalizations"] + report.SealedRows["runtime_documents"]
	if uses < sealed {
		return SnapshotInspection{}, ErrEvidence
	}
	if err = s.inspectRuntimeSemantics(ctx, tx, control); err != nil {
		return SnapshotInspection{}, inspectionContextError(ctx, err)
	}
	report.SemanticFingerprint = fp.Sum()
	if err = reader.Finish(); err != nil {
		return SnapshotInspection{}, inspectionReaderError(parent, err)
	}
	return report, nil
}

func inspectionContextError(ctx context.Context, err error) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	return err
}
func inspectionReaderError(ctx context.Context, err error) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, snapshotcheck.ErrInput) {
		return ErrInput
	}
	if errors.Is(err, snapshotcheck.ErrLimit) {
		return ErrInspectionLimit
	}
	return ErrStorage
}

func inspectionIntegrity(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return ErrStorage
	}
	n := 0
	for rows.Next() {
		var result string
		if rows.Scan(&result) != nil || result != "ok" {
			rows.Close()
			return ErrEvidence
		}
		n++
	}
	e, c := rows.Err(), rows.Close()
	if e != nil || c != nil {
		return ErrStorage
	}
	if n != 1 {
		return ErrEvidence
	}
	rows, err = tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return ErrStorage
	}
	bad := rows.Next()
	e, c = rows.Err(), rows.Close()
	if e != nil || c != nil {
		return ErrStorage
	}
	if bad {
		return ErrEvidence
	}
	return nil
}
