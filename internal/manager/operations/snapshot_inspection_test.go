package operations

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"docker-manager/internal/backup"
	"docker-manager/internal/protocol"
	"fmt"
	"runtime"
	"strings"
)

func miCopy(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copy", "snapshot.db")
	if err := backup.Snapshot(context.Background(), source, path); err != nil {
		t.Fatal(err)
	}
	return path
}
func miInspect(t *testing.T, p *recoveryPair, keys Keys) SnapshotInspection {
	t.Helper()
	path := miCopy(t, p.path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := InspectRuntimeSnapshot(context.Background(), path, p.m.Identity(), keys)
	if err != nil {
		t.Fatal("inspect valid snapshot", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("inspection changed closed copy", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err = os.Lstat(path + suffix); !os.IsNotExist(err) {
			t.Fatal("inspection created companion", suffix, err)
		}
	}
	return got
}
func TestManagerInspectionMI01CompleteReadOnlyReport(t *testing.T) {
	p, login, auth, keys := runtimePair(t)
	op := runtimeApply(t, p, login)
	if _, err := p.m.NextNodeDelivery(context.Background(), p.b, time.Now()); err != nil {
		t.Fatal(err)
	}
	boot := p.m.boot
	uses := mfUses(t, p.m)
	got := miInspect(t, p, keys)
	if got.Identity != p.m.Identity() || got.SchemaVersion != 5 || got.ProtocolVersion != 1 || got.EncryptionKeyID != keys.EncryptionID || got.EncryptionKeyUses != uint64(uses) {
		t.Fatal("wrong report identity", got)
	}
	if len(got.TableRows) != 29 || len(got.SealedRows) != 9 || got.TableRows["operations"] != 1 || got.SealedRows["runtime_deliveries"] != 1 || got.SealedRows["runtime_pending_events"] != 0 {
		t.Fatal("incomplete counts", got.TableRows, got.SealedRows)
	}
	root, err := base64.RawURLEncoding.Strict().DecodeString(got.SemanticFingerprint)
	if err != nil || len(root) != 32 || got.SemanticFormat != "dm-manager-runtime-semantic-v1" {
		t.Fatal("bad root", got.SemanticFingerprint, err)
	}
	again := miInspect(t, p, keys)
	if !reflect.DeepEqual(got, again) {
		t.Fatal("nondeterministic report")
	}
	if p.m.boot != boot || mfUses(t, p.m) != uses {
		t.Fatal("source lifecycle changed")
	}
	if _, err = p.m.Get(context.Background(), op.OperationID); err != nil {
		t.Fatal("source writer invalidated", err)
	}
	if err = p.m.RecordNodeSeen(context.Background(), auth, p.b.SessionID); err != nil {
		t.Fatal("original writer lost authority", err)
	}
}
func TestManagerInspectionMI02IdentitySchemaAndHistoricalCipher(t *testing.T) {
	p, login, _, keys := runtimePair(t)
	runtimeApply(t, p, login)
	tests := []struct {
		name, mutation string
		identity       Identity
		want           error
	}{
		{"identity", "", Identity{validationID, keys.DigestID, keys.EncryptionID}, ErrIdentity},
		{"version", `PRAGMA user_version=4`, p.m.Identity(), ErrSchema},
		{"extra_table", `CREATE TABLE hidden(x BLOB)`, p.m.Identity(), ErrSchema},
		{"cipher", `UPDATE user_sessions SET csrf_cipher=zeroblob(length(csrf_cipher))`, p.m.Identity(), ErrEvidence},
		{"claims", `DELETE FROM resource_claims WHERE resource_kind='project'`, p.m.Identity(), ErrEvidence},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := miCopy(t, p.path)
			if tc.mutation != "" {
				miMutate(t, path, tc.mutation)
			}
			got, err := InspectRuntimeSnapshot(context.Background(), path, tc.identity, keys)
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(got, SnapshotInspection{}) {
				t.Fatal("accepted invalid snapshot", got, err)
			}
		})
	}
}
func miMutate(t *testing.T, path, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(query, args...); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestManagerInspectionMI04HistoricalPendingAndRevoked(t *testing.T) {
	p, login, auth, keys := runtimePair(t)
	op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
	eventReceive(t, p.m, p.b, runtimeSeq(t, eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""), 3))
	if _, err := p.m.db.Exec(`UPDATE user_sessions SET revoked_at=1,expires_at=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.m.db.Exec(`UPDATE users SET enabled=0,auth_rev=auth_rev+1 WHERE system_admin=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.m.db.Exec(`UPDATE nodes SET session_id='',boot_id='',frozen=1`); err != nil {
		t.Fatal(err)
	}
	got := miInspect(t, p, keys)
	if got.SealedRows["runtime_pending_events"] != 1 {
		t.Fatal("pending omitted", got)
	}
}
func TestManagerInspectionMI07FinalizedAndUnreferencedAcceptedMAC(t *testing.T) {
	p, login, auth, keys := runtimePair(t)
	ctx := context.Background()
	op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
	eventReceive(t, p.m, p.b, eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""))
	request, err := p.m.PrepareFinalize(ctx, p.b, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := p.m.validator.Validate(request)
	receipt, _ := protocol.NewMessage("finalized", p.b.SessionID, env.MessageID, env.Body)
	if err = p.m.ReceiveFinalized(ctx, p.b, receipt); err != nil {
		t.Fatal(err)
	}
	miInspect(t, p, keys)
	// An accepted bit without its authentic MAC must never become release evidence.
	path := miCopy(t, p.path)
	miMutate(t, path, `UPDATE finalize_requests SET confirmation_mac=zeroblob(32)`)
	if _, err = InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys); !errors.Is(err, ErrEvidence) {
		t.Fatal("forged accepted request", err)
	}
}
func TestManagerInspectionMI08MetadataChangesRootAndMI10Canceled(t *testing.T) {
	p, _, _, keys := runtimePair(t)
	path := miCopy(t, p.path)
	first, err := InspectRuntimeSnapshot(context.Background(), path, p.m.Identity(), keys)
	if err != nil {
		t.Fatal(err)
	}
	miMutate(t, path, `UPDATE audit_events SET outcome='reviewed'`)
	second, err := InspectRuntimeSnapshot(context.Background(), path, p.m.Identity(), keys)
	if err != nil {
		t.Fatal(err)
	}
	if first.SemanticFingerprint == second.SemanticFingerprint {
		t.Fatal("ordinary metadata omitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before, _ := os.ReadFile(path)
	if got, err := InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, SnapshotInspection{}) {
		t.Fatal("canceled inspection", got, err)
	}
	after, _ := os.ReadFile(path)
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("cancel wrote source")
	}
}

func miApplyProject(t *testing.T, p *recoveryPair, login BrowserLogin, project string) Operation {
	t.Helper()
	created := replyMap(t, serviceCall(t, p.m, login.Session, "POST", "/api/v1/nodes/"+p.b.NodeID+"/stacks", map[string]any{"project_name": project, "config": serviceConfig()}, newServiceID(t), ""))
	accepted := replyMap(t, serviceCall(t, p.m, login.Session, "POST", "/api/v1/stacks/"+created["stack_id"].(string)+"/apply", map[string]any{"version_id": created["version_id"]}, newServiceID(t), ""))
	op, err := p.m.Get(context.Background(), accepted["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return op
}
func miRich(t *testing.T) (*recoveryPair, Keys) {
	t.Helper()
	p, login, auth, keys := runtimePair(t)
	ctx := context.Background()
	first := miApplyProject(t, p, login, "first")
	if d, err := p.m.NextNodeDelivery(ctx, p.b, time.Now()); err != nil || d == nil {
		t.Fatal("fixture prepare delivery", err)
	}
	first = runtimeAuthorize(t, p, auth, first)
	second := runtimeAuthorize(t, p, auth, miApplyProject(t, p, login, "second"))
	if _, err := p.m.NextNodeDelivery(ctx, p.b, time.Now()); err != nil {
		t.Fatal(err)
	}
	eventReceive(t, p.m, p.b, eventResult(t, p.m, p.b, first, "normal_exit", 0, "complete", "consistent", ""))
	request, err := p.m.PrepareFinalize(ctx, p.b, first.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := p.m.validator.Validate(request)
	receipt, _ := protocol.NewMessage("finalized", p.b.SessionID, env.MessageID, env.Body)
	if err = p.m.ReceiveFinalized(ctx, p.b, receipt); err != nil {
		t.Fatal(err)
	}
	eventReceive(t, p.m, p.b, runtimeSeq(t, eventResult(t, p.m, p.b, second, "normal_exit", 0, "complete", "consistent", ""), 4))
	eventReceive(t, p.m, p.b, eventResult(t, p.m, p.b, second, "normal_exit", 0, "error", "unknown", ""))
	unknown := second
	unknown.OperationID = newServiceID(t)
	if _, err = p.m.ReceiveRecoveryEvent(ctx, p.b, eventResult(t, p.m, p.b, unknown, "normal_exit", 0, "complete", "consistent", "")); err != nil {
		t.Fatal(err)
	}
	// The first saved package becomes historical and is no longer a head.
	var version string
	if err = p.m.db.QueryRow(`SELECT current_version_id FROM stacks WHERE id=?`, first.StackID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	serviceCall(t, p.m, login.Session, "POST", "/api/v1/stacks/"+first.StackID+"/versions", map[string]any{"config": serviceConfig()}, newServiceID(t), `"`+version+`"`)
	if _, err = p.m.Login(ctx, "admin", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if _, err = p.m.db.Exec(`UPDATE user_sessions SET revoked_at=1,expires_at=2 WHERE id=?`, login.Session.id); err != nil {
		t.Fatal(err)
	}
	return p, keys
}
func TestManagerInspectionMI03EverySealedTableHistoricalTamper(t *testing.T) {
	p, keys := miRich(t)
	report := miInspect(t, p, keys)
	cases := []struct{ table, body string }{
		{"operations", "payload_cipher"}, {"operation_events", "body_cipher"}, {"runtime_pending_events", "body_cipher"}, {"recovery_pages", "body_cipher"}, {"recovery_cases", "evidence_cipher"}, {"finalization", "body_cipher"}, {"user_sessions", "csrf_cipher"}, {"config_versions", "package_cipher"}, {"runtime_deliveries", "body_cipher"},
	}
	for _, tc := range cases {
		if report.SealedRows[tc.table] == 0 {
			t.Fatal("missing real sealed fixture", tc.table)
		}
		for _, field := range []string{tc.body, "wrapped_dek", "fingerprint", "key_id"} {
			t.Run(tc.table+"/"+field, func(t *testing.T) {
				path := miCopy(t, p.path)
				replacement := "zeroblob(length(" + field + "))"
				if field == "key_id" {
					replacement = "'wrong-key'"
				}
				miMutate(t, path, "UPDATE "+tc.table+" SET "+field+"="+replacement+" WHERE rowid=(SELECT rowid FROM "+tc.table+" ORDER BY rowid DESC LIMIT 1)")
				got, err := InspectRuntimeSnapshot(context.Background(), path, p.m.Identity(), keys)
				if !errors.Is(err, ErrEvidence) || !reflect.DeepEqual(got, SnapshotInspection{}) {
					t.Fatal("historical tamper accepted", got, err)
				}
			})
		}
	}
}
func TestManagerInspectionMI06CollectingAbandonedAndUnknownRecord(t *testing.T) {
	p, _, _, keys := runtimePair(t)
	ctx := context.Background()
	req, err := p.m.StartRecovery(ctx, p.b, "all", 1)
	if err != nil {
		t.Fatal(err)
	}
	miInspect(t, p, keys) // committed outgoing request, no page yet
	// A valid unknown record can be durably collected before ProcessRecovery.
	op := Operation{OperationID: newServiceID(t), NodeID: p.b.NodeID, LedgerID: p.b.LedgerID, StackID: newServiceID(t), VersionID: newServiceID(t), ProjectName: "unknown_project", Digest: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))}
	record := map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "stack_id": op.StackID, "version_id": op.VersionID, "project_name": op.ProjectName, "phase": "ready", "last_event_seq": 0, "result_event_seq": nil, "latest_result": nil, "claims_held": true, "report_acked": false, "final_state": nil, "finalized_result_event_seq": nil}
	raw := recoveryPageReply(t, p.m, p.b, req, 1, []any{record}, nil)
	if _, err = p.m.ReceiveRecoveryPage(ctx, p.b, raw); err != nil {
		t.Fatal(err)
	}
	miInspect(t, p, keys)
	if _, err = p.m.ProcessRecovery(ctx, p.b); err != nil {
		t.Fatal(err)
	}
	miInspect(t, p, keys)
	if _, err = p.m.db.Exec(`UPDATE recovery_scans SET status='abandoned'`); err != nil {
		t.Fatal(err)
	}
	miInspect(t, p, keys)
}

func TestManagerInspectionMI09NewKEKPreservesAllLogicalRows(t *testing.T) {
	p, keys := miRich(t)
	path := miCopy(t, p.path)
	ctx := context.Background()
	original, err := InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	view := &Store{version: 5, identity: p.m.Identity(), keys: keys, validator: p.m.validator}
	inspector := &snapshotInspector{ctx: ctx, tx: tx, view: view}
	type record struct {
		table   inspectionTable
		row     inspectionRow
		binding blobBinding
		raw     []byte
	}
	var records []record
	for _, table := range inspectionTables {
		if table.body == "" {
			continue
		}
		columns := strings.Split(table.columns, ",")
		rows, err := tx.Query("SELECT " + table.columns + " FROM " + table.name)
		if err != nil {
			t.Fatal(err)
		}
		var collected []inspectionRow
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(values))
			for n := range values {
				dest[n] = &values[n]
			}
			if err = rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			r := inspectionRow{}
			for n, c := range columns {
				r[c] = values[n]
			}
			collected = append(collected, r)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		for _, r := range collected {
			raw, err := inspector.row(table, r)
			if err != nil {
				t.Fatal(table.name, err)
			}
			records = append(records, record{table, r, miBinding(t, inspector, table, r), raw})
		}
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	newKeys := storageKeys()
	newKeys.EncryptionID = "replacement-encryption-key"
	newKeys.EncryptionKey = bytes.Repeat([]byte{73}, 32)
	newIdentity := p.m.Identity()
	newIdentity.EncryptionKeyID = newKeys.EncryptionID
	newView := &Store{identity: newIdentity, keys: newKeys}
	for _, record := range records {
		dek := make([]byte, 32)
		if _, err = rand.Read(dek); err != nil {
			t.Fatal(err)
		}
		body, _ := blobAEAD(dek)
		wrap, _ := blobAEAD(newKeys.EncryptionKey)
		cipher := body.Seal(nil, nil, record.raw, newView.blobAAD("dm-manager-body-v1", record.binding, newKeys.EncryptionID))
		wrapped := wrap.Seal(nil, nil, dek, newView.blobAAD("dm-manager-dek-v1", record.binding, newKeys.EncryptionID))
		clear(dek)
		clear(record.raw)
		pks := strings.Split(record.table.pk, ",")
		pred := make([]string, len(pks))
		args := []any{cipher, wrapped, newKeys.EncryptionID}
		for n, k := range pks {
			pred[n] = k + "=?"
			args = append(args, record.row[k])
		}
		if _, err = db.Exec("UPDATE "+record.table.name+" SET "+record.table.body+"=?,wrapped_dek=?,key_id=? WHERE "+strings.Join(pred, " AND "), args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`UPDATE keys SET encryption_key_id=?,encryption_key_check=?,uses=1048576`, newKeys.EncryptionID, keyCheck(newIdentity.ManagerID, "dm-manager-encryption-key-v1", newKeys.EncryptionID, newKeys.EncryptionKey)); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	changed, err := InspectRuntimeSnapshot(ctx, path, newIdentity, newKeys)
	if err != nil {
		t.Fatal("new KEK logical copy", err)
	}
	if changed.SemanticFingerprint != original.SemanticFingerprint || !reflect.DeepEqual(changed.TableRows, original.TableRows) || !reflect.DeepEqual(changed.SealedRows, original.SealedRows) {
		t.Fatal("KEK material affected semantic comparison")
	}
}

func miBinding(t *testing.T, i *snapshotInspector, table inspectionTable, r inspectionRow) blobBinding {
	t.Helper()
	s := i.view
	ctx, q := i.ctx, i.tx
	switch table.name {
	case "operations", "operation_events", "runtime_pending_events", "finalization":
		op, err := s.readOperation(ctx, q, r.s("operation_id"))
		if err != nil {
			t.Fatal(err)
		}
		switch table.name {
		case "operations":
			return blobBinding{op.OperationID, op.NodeID, op.Digest, "payload", 0}
		case "operation_events":
			return blobBinding{op.OperationID, op.NodeID, op.Digest, r.s("kind"), uint64(r.n("event_seq"))}
		case "runtime_pending_events":
			return blobBinding{op.OperationID, op.NodeID, op.Digest, "result", uint64(r.n("event_seq"))}
		default:
			req, err := s.readFinalizeRequest(ctx, q, r.s("request_id"), op)
			if err != nil {
				t.Fatal(err)
			}
			return finalizationBinding(op, req)
		}
	case "recovery_pages":
		scan, err := s.readRecoveryScanByID(ctx, q, r.s("round_id"))
		if err != nil {
			t.Fatal(err)
		}
		req, err := s.readRecoveryRequest(ctx, q, scan, r.s("request_id"))
		if err != nil {
			t.Fatal(err)
		}
		return recoveryPageBinding(scan, int(r.n("page_no")), req.Cursor)
	case "recovery_cases":
		return recoveryCaseBinding(RecoveryCase{r.s("id"), r.s("node_id"), r.s("ledger_id"), r.s("operation_id"), r.s("kind"), r.s("round_id")})
	case "user_sessions":
		return blobBinding{r.s("id"), r.s("user_id"), base64.RawURLEncoding.EncodeToString(r.b("token_hash")), "session-csrf", 0}
	case "config_versions":
		st, err := readServiceStack(ctx, q, r.s("stack_id"))
		if err != nil {
			t.Fatal(err)
		}
		return blobBinding{r.s("id"), st.node, st.id, "config", 0}
	case "runtime_deliveries":
		return deliveryBinding(Binding{r.s("node_id"), r.s("ledger_id"), r.s("session_id")}, Delivery{MessageID: r.s("message_id"), OperationID: r.s("operation_id"), Kind: r.s("kind"), Generation: uint64(r.n("generation"))}, r.s("manager_boot"), r.s("round_id"))
	}
	t.Fatal("unhandled sealed table", table.name)
	return blobBinding{}
}

func TestManagerInspectionMI02KeyFKAndQuotaBoundaries(t *testing.T) {
	p, login, _, keys := runtimePair(t)
	runtimeApply(t, p, login)
	ctx := context.Background()
	for _, which := range []string{"digest", "encryption"} {
		t.Run(which, func(t *testing.T) {
			wrong := storageKeys()
			if which == "digest" {
				wrong.DigestKey = bytes.Repeat([]byte{88}, 32)
			} else {
				wrong.EncryptionKey = bytes.Repeat([]byte{89}, 32)
			}
			path := miCopy(t, p.path)
			if got, err := InspectRuntimeSnapshot(ctx, path, p.m.Identity(), wrong); !errors.Is(err, ErrKey) || !reflect.DeepEqual(got, SnapshotInspection{}) {
				t.Fatal("wrong key accepted", got, err)
			}
		})
	}
	for _, tc := range []struct {
		name, sql string
		want      error
	}{
		{"digest_check", `UPDATE keys SET digest_key_check=zeroblob(32)`, ErrKey},
		{"quota_low", `UPDATE keys SET uses=0`, ErrEvidence},
		{"index", `DROP INDEX operations_page`, ErrSchema},
		{"foreign_key", `UPDATE operations SET version_id='00000000-0000-4000-8000-000000000000'`, ErrEvidence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := miCopy(t, p.path)
			miMutate(t, path, tc.sql)
			if _, err := InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys); !errors.Is(err, tc.want) {
				t.Fatal("boundary accepted", err)
			}
		})
	}
	path := miCopy(t, p.path)
	miMutate(t, path, `UPDATE keys SET uses=1048576`)
	got, err := InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys)
	if err != nil || got.EncryptionKeyUses != 1048576 {
		t.Fatal("exhausted key refused read", got, err)
	}
}
func TestManagerInspectionMI07EveryAcceptedRequestAndBlockedIntention(t *testing.T) {
	p, login, auth, keys := runtimePair(t)
	ctx := context.Background()
	op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
	result := eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", "")
	eventReceive(t, p.m, p.b, result)
	first, err := p.m.PrepareFinalize(ctx, p.b, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := p.m.validator.Validate(first)
	receipt, _ := protocol.NewMessage("finalized", p.b.SessionID, env.MessageID, env.Body)
	if err = p.m.ReceiveFinalized(ctx, p.b, receipt); err != nil {
		t.Fatal(err)
	}
	// A real second historical context is authenticated separately and is not
	// referenced by the original finalization row.
	secondSession := newServiceID(t)
	secondRaw, _ := protocol.NewMessage("finalize", secondSession, "", json.RawMessage(env.Body))
	secondEnv, _ := p.m.validator.Validate(secondRaw)
	request := finalizeRequest{ID: secondEnv.MessageID, OperationID: op.OperationID, NodeID: op.NodeID, LedgerID: op.LedgerID, SessionID: secondSession, ManagerBoot: p.m.boot, AgentBoot: p.agentBoot, Raw: secondRaw, Accepted: true}
	body, _ := parseFinalization(secondEnv.Body)
	mac, _ := p.m.finalizeConfirmationMAC(request, body)
	if _, err = p.m.db.Exec(`INSERT INTO finalize_requests(message_id,operation_id,node_id,ledger_id,session_id,manager_boot,agent_boot,round_id,raw,accepted,confirmation_mac) VALUES(?,?,?,?,?,?,?,'',?,1,?)`, request.ID, op.OperationID, op.NodeID, op.LedgerID, secondSession, p.m.boot, p.agentBoot, secondRaw, mac); err != nil {
		t.Fatal(err)
	}
	miInspect(t, p, keys)
	path := miCopy(t, p.path)
	miMutate(t, path, `UPDATE finalize_requests SET confirmation_mac=zeroblob(32) WHERE message_id=?`, request.ID)
	if _, err = InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys); !errors.Is(err, ErrEvidence) {
		t.Fatal("nonpointer accepted MAC skipped", err)
	}

	p2, login2, auth2, keys2 := runtimePair(t)
	op2 := runtimeAuthorize(t, p2, auth2, runtimeApply(t, p2, login2))
	eventReceive(t, p2.m, p2.b, eventResult(t, p2.m, p2.b, op2, "normal_exit", 0, "complete", "consistent", ""))
	if _, err = p2.m.PrepareFinalize(ctx, p2.b, op2.OperationID); err != nil {
		t.Fatal(err)
	}
	// A later conflicting process fact preserves the earlier unsent intention.
	conflicting := eventResult(t, p2.m, p2.b, op2, "normal_exit", 7, "complete", "consistent", "")
	if _, err = p2.m.ReceiveEvent(ctx, p2.b, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	miInspect(t, p2, keys2)
}
func TestManagerInspectionMI10RowBudgetAndLateRow(t *testing.T) {
	p, _, _, keys := runtimePair(t)
	ctx := context.Background()
	// Clone complete real audit metadata, changing only the immutable record ID.
	for n := 0; n < 130; n++ {
		if _, err := p.m.db.Exec(`INSERT INTO audit_events(id,user_id,node_id,action,target_type,target_id,operation_id,created_at,outcome) SELECT ?,user_id,node_id,action,target_type,target_id,operation_id,created_at,outcome FROM audit_events LIMIT 1`, newServiceID(t)); err != nil {
			t.Fatal(err)
		}
	}
	got := miInspect(t, p, keys)
	if got.TableRows["audit_events"] < 130 {
		t.Fatal("late rows skipped")
	}
	path := miCopy(t, p.path)
	miMutate(t, path, `UPDATE audit_events SET target_id='invalid' WHERE id=(SELECT max(id) FROM audit_events)`)
	if _, err := InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys); !errors.Is(err, ErrEvidence) {
		t.Fatal("late ordinary metadata skipped", err)
	}
	path = miCopy(t, p.path)
	miMutate(t, path, `UPDATE audit_events SET action=CAST(zeroblob(33554433) AS TEXT) WHERE id=(SELECT min(id) FROM audit_events)`)
	if got, err := InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys); !errors.Is(err, ErrInspectionLimit) || !reflect.DeepEqual(got, SnapshotInspection{}) {
		t.Fatal("row budget not enforced", got, err)
	}
}

func TestManagerInspectionMI07HistoricalAbsenceRelease(t *testing.T) {
	p, login, _, keys := runtimePair(t)
	ctx := context.Background()
	op := runtimeApply(t, p, login)
	riScan(t, p, "all", 10)
	riComplete(t, p)
	got, err := p.m.Get(ctx, op.OperationID)
	if err != nil || got.ReleaseStatus != "released" || got.LatestResultSeq != 0 {
		t.Fatal("absence fixture", got, err)
	}
	report := miInspect(t, p, keys)
	if report.TableRows["absence_releases"] != 1 {
		t.Fatal("absence not checked")
	}
	// A new scan abandons the earlier completed evidence without invalidating it.
	if _, err = p.m.StartRecovery(ctx, p.b, "all", 10); err != nil {
		t.Fatal(err)
	}
	miInspect(t, p, keys)
	path := miCopy(t, p.path)
	miMutate(t, path, `UPDATE recovery_expected SET last_event_seq=1 WHERE operation_id=?`, op.OperationID)
	if _, err = InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys); !errors.Is(err, ErrEvidence) {
		t.Fatal("forged absence watermark", err)
	}
}
func TestManagerInspectionMI05AuthenticatedBodySchemaAndOriginalChain(t *testing.T) {
	p, login, auth, keys := runtimePair(t)
	ctx := context.Background()
	op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
	old := eventResult(t, p.m, p.b, op, "normal_exit", 0, "error", "unknown", "")
	eventReceive(t, p.m, p.b, old)
	newer := runtimeSeq(t, eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""), 3)
	eventReceive(t, p.m, p.b, newer)
	miInspect(t, p, keys)
	raw, err := p.m.readPayload(ctx, p.m.db, op)
	if err != nil {
		t.Fatal(err)
	}
	broken := append([]byte(nil), raw...)
	broken = bytes.Replace(broken, []byte(`"stack.apply"`), []byte(`"stack.invalid"`), 1)
	clear(raw)
	binding := blobBinding{op.OperationID, op.NodeID, op.Digest, "payload", 0}
	sealed, err := p.m.seal(ctx, binding, broken)
	clear(broken)
	if err != nil {
		t.Fatal(err)
	}
	path := miCopy(t, p.path)
	miMutate(t, path, `UPDATE operations SET payload_cipher=?,wrapped_dek=?,fingerprint=? WHERE operation_id=?`, sealed.Cipher, sealed.WrappedDEK, sealed.Fingerprint, op.OperationID)
	if _, err = InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys); !errors.Is(err, ErrEvidence) {
		t.Fatal("authenticated invalid input accepted", err)
	}
	// Directly sealed malformed Config tests Schema after both AEADs and body MAC.
	var version, node, stack string
	if err = p.m.db.QueryRow(`SELECT v.id,s.node_id,s.id FROM config_versions v JOIN stacks s ON s.id=v.stack_id LIMIT 1`).Scan(&version, &node, &stack); err != nil {
		t.Fatal(err)
	}
	sealed, err = p.m.seal(ctx, blobBinding{version, node, stack, "config", 0}, []byte(`{"files":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	path = miCopy(t, p.path)
	miMutate(t, path, `UPDATE config_versions SET package_cipher=?,wrapped_dek=?,fingerprint=? WHERE id=?`, sealed.Cipher, sealed.WrappedDEK, sealed.Fingerprint, version)
	if _, err = InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys); !errors.Is(err, ErrEvidence) {
		t.Fatal("authenticated invalid Config accepted", err)
	}
}

func TestManagerInspectionMI06CaseRoundEvidenceMustBelongToItsScan(t *testing.T) {
	for _, kind := range []string{"unknown_operation", "page_conflict"} {
		t.Run(kind, func(t *testing.T) {
			p, _, _, keys := runtimePair(t)
			ctx := context.Background()
			var round string
			if err := p.m.db.QueryRow(`SELECT active_round FROM recovery_nodes WHERE node_id=?`, p.b.NodeID).Scan(&round); err != nil {
				t.Fatal(err)
			}
			id := newServiceID(t)
			var body any
			if kind == "unknown_operation" {
				r := recoveryRootRecord()
				r["operation_id"] = id
				body = r
			} else {
				body = map[string]any{"scope": "all", "scan_id": id, "ledger_id": p.b.LedgerID, "total": 0, "records": []any{}, "next_cursor": nil, "complete": true, "error": nil}
			}
			raw, _ := json.Marshal(body)
			if err := p.m.putRecoveryCase(ctx, p.b, round, id, kind, raw); err != nil {
				t.Fatal(err)
			}
			path := miCopy(t, p.path)
			if _, err := InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys); !errors.Is(err, ErrEvidence) {
				t.Fatal("authenticated case attached to unrelated scan", err)
			}
		})
	}
	t.Run("legitimate_page_conflict", func(t *testing.T) {
		p, _, _, keys := runtimePair(t)
		ctx := context.Background()
		var request []byte
		if err := p.m.db.QueryRow(`SELECT raw FROM recovery_requests WHERE kind='reconcile' LIMIT 1`).Scan(&request); err != nil {
			t.Fatal(err)
		}
		reply := recoveryPageReply(t, p.m, p.b, request, 0, []any{}, nil)
		// The real writer stores this page as a conflict, with case.operation_id
		// naming the original scan rather than the conflicting incoming scan.
		if _, err := p.m.ReceiveRecoveryPage(ctx, p.b, reply); !errors.Is(err, ErrConflict) {
			t.Fatal("real page conflict", err)
		}
		miInspect(t, p, keys)
	})
}

func TestManagerInspectionMI04CommittedPendingBeforePromotion(t *testing.T) {
	for _, mode := range []string{"complete", "contradiction"} {
		t.Run(mode, func(t *testing.T) {
			p, login, auth, keys := runtimePair(t)
			ctx := context.Background()
			op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
			high := runtimeSeq(t, eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""), 3)
			if _, err := p.m.ReceiveRecoveryEvent(ctx, p.b, high); err != nil {
				t.Fatal(err)
			}
			exit := 0
			if mode == "contradiction" {
				exit = 7
			}
			base := eventResult(t, p.m, p.b, op, "normal_exit", exit, "complete", "consistent", "")
			if _, err := p.m.ReceiveRecoveryEvent(ctx, p.b, base); err != nil {
				t.Fatal(err)
			}
			got, err := p.m.Get(ctx, op.OperationID)
			if err != nil || got.LatestResultSeq != 2 || got.Blocked {
				t.Fatal("invalid source transaction boundary", got, err)
			}
			report := miInspect(t, p, keys)
			if report.SealedRows["runtime_pending_events"] != 1 {
				t.Fatal("pending disappeared")
			}
		})
	}
}
func TestManagerInspectionMI10ClaimsRejectedBeforeAllocation(t *testing.T) {
	p, login, _, keys := runtimePair(t)
	op := runtimeApply(t, p, login)
	ctx := context.Background()
	tx, err := p.m.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 1024; n++ {
		if _, err = tx.Exec(`INSERT INTO resource_claims(node_id,resource_kind,resource_id,operation_id) VALUES(?,'project',?,?)`, op.NodeID, fmt.Sprintf("extra-%04d-", n)+strings.Repeat("x", 65536), op.OperationID); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	path := miCopy(t, p.path)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = InspectRuntimeSnapshot(ctx, path, p.m.Identity(), keys)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrEvidence) {
		t.Fatal(err)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > 32<<20 {
		t.Fatalf("unbounded invalid claims loaded before rejection: %d bytes", allocated)
	}
	t.Logf("invalid claims rejected with %d allocated bytes; includes SQLite and hashing overhead", allocated)
}

func TestManagerInspectionMI04DisabledOwnerAndRevokedNodeHistory(t *testing.T) {
	p, admin, auth, keys := runtimePair(t)
	ctx := context.Background()
	created := replyMap(t, serviceCall(t, p.m, admin.Session, "POST", "/api/v1/users", map[string]any{"username": "worker", "password": "worker test password"}, newServiceID(t), ""))
	user := created["user_id"].(string)
	serviceCall(t, p.m, admin.Session, "PUT", "/api/v1/nodes/"+p.b.NodeID+"/grants/"+user, map[string]any{"capabilities": []string{"node.read", "config.read", "config.write", "stack.deploy", "operation.read", "operation.output.read", "operation.cancel"}}, newServiceID(t), `"0"`)
	worker, err := p.m.Login(ctx, "worker", "worker test password")
	if err != nil {
		t.Fatal(err)
	}
	op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, worker))
	serviceCall(t, p.m, admin.Session, "PATCH", "/api/v1/users/"+user, map[string]any{"enabled": false}, newServiceID(t), `"1"`)
	serviceCall(t, p.m, admin.Session, "POST", "/api/v1/nodes/"+p.b.NodeID+"/revoke", map[string]any{}, newServiceID(t), "")
	var enabled bool
	var revoked sql.NullInt64
	if err = p.m.db.QueryRow(`SELECT u.enabled,n.revoked_at FROM users u,nodes n WHERE u.id=? AND n.id=?`, user, p.b.NodeID).Scan(&enabled, &revoked); err != nil || enabled || !revoked.Valid {
		t.Fatal("fixture did not revoke actual owner/node", err)
	}
	got := miInspect(t, p, keys)
	if got.TableRows["operations"] != 1 || got.SealedRows["user_sessions"] != 2 || op.AuthRev == 0 {
		t.Fatal("historical owner evidence omitted", got)
	}
}
