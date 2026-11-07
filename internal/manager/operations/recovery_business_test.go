package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"docker-manager/internal/protocol"
)

func recoveryRootRecord() map[string]any {
	return map[string]any{"operation_id": storageManagerID, "input_digest": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "stack_id": ctlUser, "version_id": ctlOther, "project_name": "fixture", "phase": "ready", "last_event_seq": 0, "result_event_seq": nil, "latest_result": nil, "claims_held": true, "report_acked": false, "final_state": nil, "finalized_result_event_seq": nil}
}

func rootRecordFor(op Operation) map[string]any {
	r := recoveryRootRecord()
	r["operation_id"] = op.OperationID
	r["input_digest"] = op.Digest
	r["stack_id"] = op.StackID
	r["version_id"] = op.VersionID
	r["project_name"] = op.ProjectName
	return r
}

func rootReceiveRecord(t *testing.T, p *recoveryPair, r map[string]any) RecoveryState {
	t.Helper()
	ctx := context.Background()
	request, err := p.m.StartRecovery(ctx, p.b, "all", 10)
	if err != nil {
		t.Fatal(err)
	}
	env := mfEnvelope(t, request)
	raw, err := protocol.NewMessage("reconcile_reply", p.b.SessionID, env.MessageID, map[string]any{"scope": "all", "scan_id": riID(9901), "ledger_id": p.b.LedgerID, "total": 1, "next_cursor": nil, "complete": true, "error": nil, "records": []any{r}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := p.m.ReceiveRecoveryPage(ctx, p.b, raw)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRecoveryRootReleasedEventFinalTransactionCannotOverwriteAbsence(t *testing.T) {
	p := riOpenPair(t)
	ctx := context.Background()
	op, _ := riPrepare(t, p, 920, false)
	raw := eventResult(t, p.m, p.b, op, "not_started", 0, "", "", "canceled_before_start")
	env := mfEnvelope(t, raw)
	parsed, err := parseEvent(env.Body)
	if err != nil {
		t.Fatal(err)
	}
	needs, err := p.m.receiveEvent(ctx, p.b, env, parsed, nil)
	if err != nil || !needs {
		t.Fatalf("event preflight %t %v", needs, err)
	}
	sealed, err := p.m.seal(ctx, blobBinding{OperationID: op.OperationID, NodeID: p.b.NodeID, Digest: op.Digest, Kind: "result", Seq: 1}, env.Body)
	if err != nil {
		t.Fatal(err)
	}
	riScan(t, p, "all", 10)
	riComplete(t, p)
	if _, err = p.m.receiveEvent(ctx, p.b, env, parsed, &sealed); err == nil {
		t.Fatal("event preflight before absence overwrote released cancellation")
	}
	got, err := p.m.Get(ctx, op.OperationID)
	if err != nil || got.ReleaseStatus != "released" || got.MaxEventSeq != 0 {
		t.Fatalf("changed cancellation %+v %v", got, err)
	}
	ack, err := p.m.ReceiveEvent(ctx, p.b, raw)
	if err != nil || len(ack) == 0 {
		t.Fatalf("quarantine ack %v", err)
	}
	riFrozen(t, p.m, p.b.NodeID, true)
	got, err = p.m.Get(ctx, op.OperationID)
	if err != nil || got.State != "canceled" || got.MaxEventSeq != 0 {
		t.Fatalf("quarantine rewrote outcome %+v %v", got, err)
	}
}

func TestRecoveryRootUnpermittedStartedEvidenceSurvivesNewEmptyScan(t *testing.T) {
	p := riOpenPair(t)
	ctx := context.Background()
	op, _ := riPrepare(t, p, 921, false)
	r := rootRecordFor(op)
	r["phase"] = "START_COMMITTED"
	r["last_event_seq"] = 1
	rootReceiveRecord(t, p, r)
	state, err := p.m.ProcessRecovery(ctx, p.b)
	if err != nil || state.Status != "blocked" {
		t.Fatalf("unpermitted Agent start not isolated %+v %v", state, err)
	}
	p.b.SessionID = riID(9921)
	riBind(t, p)
	riScan(t, p, "all", 10)
	state, err = p.m.ProcessRecovery(ctx, p.b)
	if err != nil || state.Status != "blocked" {
		t.Fatalf("lost earlier Agent start %+v %v", state, err)
	}
	claims, err := p.m.Claims(ctx, op.OperationID)
	if err != nil || len(claims) != 2 {
		t.Fatalf("released unknown start %v", err)
	}
}

func TestRecoveryRootAbsenceThenReadyRecordPreservesHistoricalOutcome(t *testing.T) {
	p := riOpenPair(t)
	ctx := context.Background()
	op, prepare := riPrepare(t, p, 922, false)
	riScan(t, p, "all", 10)
	riComplete(t, p)
	if _, err := p.a.RecordReadyFixture(ctx, prepare); err != nil {
		t.Fatal(err)
	}
	riScan(t, p, "all", 10)
	state, err := p.m.ProcessRecovery(ctx, p.b)
	if err != nil || state.Status != "blocked" {
		t.Fatalf("late ready ignored %+v %v", state, err)
	}
	got, err := p.m.Get(ctx, op.OperationID)
	if err != nil || got.State != "canceled" || got.ReleaseStatus != "released" {
		t.Fatalf("old outcome changed %+v %v", got, err)
	}
	if _, err = p.m.PrepareRecoveryComplete(ctx, p.b); err == nil {
		t.Fatal("late ready allowed completion")
	}
}

func TestRecoveryRootCaseQuotaFailureStillQuarantinesOpenedNode(t *testing.T) {
	p := riOpenPair(t)
	ctx := context.Background()
	op, _ := riPrepare(t, p, 924, false)
	if err := p.m.RecordReadyFixture(ctx, p.b, op.OperationID, op.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := p.m.db.Exec(`UPDATE keys SET uses=? WHERE singleton=1`, maxKeyUses); err != nil {
		t.Fatal(err)
	}
	unknown := Operation{OperationID: riID(9922), Digest: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	raw := eventResult(t, p.m, p.b, unknown, "not_started", 0, "", "", "canceled_before_start")
	if ack, err := p.m.ReceiveRecoveryEvent(ctx, p.b, raw); !errors.Is(err, ErrKeyExhausted) || len(ack) != 0 {
		t.Fatalf("case exhaustion ack=%d err=%v", len(ack), err)
	}
	riFrozen(t, p.m, p.b.NodeID, true)
	var quarantined bool
	if err := p.m.db.QueryRow(`SELECT quarantined FROM recovery_nodes WHERE node_id=?`, p.b.NodeID).Scan(&quarantined); err != nil || !quarantined {
		t.Fatalf("lost durable quarantine %v", err)
	}
	request, _ := protocol.NewMessage("request_start", p.b.SessionID, "", map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "start_nonce": riID(9933)})
	if _, err := p.m.AuthorizeStart(ctx, p.b, request); !errors.Is(err, ErrFrozen) {
		t.Fatalf("quarantined ready authorized %v", err)
	}
}

func TestRecoveryRootCaseFreezeFailureStillBlocksLocalPermit(t *testing.T) {
	p := riOpenPair(t)
	ctx := context.Background()
	op, _ := riPrepare(t, p, 925, false)
	if err := p.m.RecordReadyFixture(ctx, p.b, op.OperationID, op.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := p.m.db.Exec(`CREATE TEMP TRIGGER fail_quarantine BEFORE UPDATE OF quarantined ON recovery_nodes BEGIN SELECT RAISE(ABORT,'controlled freeze failure'); END`); err != nil {
		t.Fatal(err)
	}
	unknown := Operation{OperationID: riID(9925), Digest: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	raw := eventResult(t, p.m, p.b, unknown, "not_started", 0, "", "", "canceled_before_start")
	if ack, err := p.m.ReceiveRecoveryEvent(ctx, p.b, raw); !errors.Is(err, ErrStorage) || len(ack) != 0 {
		t.Fatalf("failed freeze returned ack %v", err)
	}
	riFrozen(t, p.m, p.b.NodeID, false) // SQL rolled back; local admission must be stricter.
	request, _ := protocol.NewMessage("request_start", p.b.SessionID, "", map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "start_nonce": riID(9934)})
	if _, err := p.m.AuthorizeStart(ctx, p.b, request); !errors.Is(err, ErrFrozen) {
		t.Fatalf("local quarantine bypassed %v", err)
	}
}

func TestRecoveryRootStartedPageConflictsWithLateNotStarted(t *testing.T) {
	p := riOpenPair(t)
	ctx := context.Background()
	op, _ := riPrepare(t, p, 926, true)
	starts := 0
	op = riAuthorize(t, p, op, &starts)
	r := rootRecordFor(op)
	r["phase"] = "START_COMMITTED"
	r["last_event_seq"] = 1
	rootReceiveRecord(t, p, r)
	if ack, err := p.m.ReceiveRecoveryEvent(ctx, p.b, eventResult(t, p.m, p.b, op, "not_started", 0, "", "", "canceled_before_start")); !errors.Is(err, ErrConflict) || len(ack) != 0 {
		t.Fatalf("contradictory result acknowledged %v", err)
	}
	state, err := p.m.ProcessRecovery(ctx, p.b)
	if err != nil || state.Status != "blocked" {
		t.Fatalf("contradictory seq1 was accepted %+v %v", state, err)
	}
	got, err := p.m.Get(ctx, op.OperationID)
	if err != nil || !got.Blocked || got.ReleaseStatus != "held" {
		t.Fatalf("terminal conflict not isolated %+v %v", got, err)
	}
	intents, err := p.m.Intents(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	for _, intent := range intents {
		if intent.Kind == "finalize" && intent.State != "suppressed" {
			t.Fatal("conflicting finalize remained active")
		}
	}
}

func TestRecoveryRootNotStartedAfterProcessedStartPageIsRejected(t *testing.T) {
	for _, processed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-business", true: "after-business"}[processed], func(t *testing.T) {
			p := riOpenPair(t)
			ctx := context.Background()
			op, _ := riPrepare(t, p, 929, true)
			starts := 0
			op = riAuthorize(t, p, op, &starts)
			r := rootRecordFor(op)
			r["phase"] = "START_COMMITTED"
			r["last_event_seq"] = 1
			rootReceiveRecord(t, p, r)
			if processed {
				state, err := p.m.ProcessRecovery(ctx, p.b)
				if err != nil || state.Status != "waiting" {
					t.Fatalf("start wait %+v %v", state, err)
				}
			}
			ack, err := p.m.ReceiveRecoveryEvent(ctx, p.b, eventResult(t, p.m, p.b, op, "not_started", 0, "", "", "canceled_before_start"))
			if !errors.Is(err, ErrConflict) || len(ack) != 0 {
				t.Fatalf("known started accepted not_started ack=%d err=%v", len(ack), err)
			}
			got, err := p.m.Get(ctx, op.OperationID)
			if err != nil || !got.Blocked || got.LatestResultSeq != 0 || got.ReleaseStatus != "held" {
				t.Fatalf("conflict projected a result %+v %v", got, err)
			}
		})
	}
}

func TestRecoveryRootReconnectKeepsNewResultWithOldAgentSnapshot(t *testing.T) {
	for _, change := range []string{"session", "manager-boot"} {
		t.Run(change, func(t *testing.T) {
			p := riOpenPair(t)
			ctx := context.Background()
			op, _ := riPrepare(t, p, 930, true)
			starts := 0
			op = riAuthorize(t, p, op, &starts)
			before := riScan(t, p, "all", 10)
			result, err := p.a.RecordResultFixture(ctx, eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""))
			if err != nil {
				t.Fatal(err)
			}
			riDeliverEvent(t, p, result)
			riAssertWaiting(t, p, op.OperationID)
			if change == "session" {
				p.b.SessionID = riID(9930)
			} else {
				identity := p.m.Identity()
				if err := p.m.Close(); err != nil {
					t.Fatal(err)
				}
				p.m, err = OpenRecovery(ctx, p.path, identity, mfKeys())
				if err != nil {
					t.Fatal(err)
				}
			}
			riBind(t, p)
			after := riScan(t, p, "all", 10)
			if after.ScanID != before.ScanID {
				t.Fatal("fixture did not reuse same Agent snapshot")
			}
			state, err := p.m.ProcessRecovery(ctx, p.b)
			if err != nil || state.Status != "waiting" {
				t.Fatalf("old snapshot falsely isolated %+v %v", state, err)
			}
			got, err := p.m.Get(ctx, op.OperationID)
			if err != nil || got.State != "succeeded" || got.Blocked || got.LatestResultSeq != 2 || got.ReleaseStatus != "pending" {
				t.Fatalf("newer result lost %+v %v", got, err)
			}
			cases, err := p.m.RecoveryCases(ctx, p.b.NodeID)
			if err != nil || len(cases) != 0 {
				t.Fatalf("false rollback case %v", err)
			}
			claims, err := p.m.Claims(ctx, op.OperationID)
			if err != nil || len(claims) != 2 {
				t.Fatalf("lost claims %v", err)
			}
		})
	}
}

func TestRecoveryRootUnsupportedHistoryShapesAreQuarantined(t *testing.T) {
	for _, shape := range []string{"started-at-two", "result-one-watermark-two", "real-result-three"} {
		t.Run(shape, func(t *testing.T) {
			p := riOpenPair(t)
			ctx := context.Background()
			op, _ := riPrepare(t, p, 927, true)
			starts := 0
			op = riAuthorize(t, p, op, &starts)
			r := rootRecordFor(op)
			if shape == "real-result-three" {
				raw := eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", "")
				var message map[string]any
				_ = json.Unmarshal(raw, &message)
				message["body"].(map[string]any)["event_seq"] = 3
				raw, _ = json.Marshal(message)
				if ack, err := p.m.ReceiveRecoveryEvent(ctx, p.b, raw); err != nil || len(ack) == 0 {
					t.Fatalf("unsupported event was not durably acknowledged %v", err)
				}
			} else {
				r["last_event_seq"] = 2
				if shape == "started-at-two" {
					r["phase"] = "START_COMMITTED"
				} else {
					r["phase"] = "result"
					r["result_event_seq"] = 1
					r["latest_result"] = json.RawMessage(mfEnvelope(t, eventResult(t, p.m, p.b, op, "not_started", 0, "", "", "canceled_before_start")).Body)
				}
				rootReceiveRecord(t, p, r)
				state, err := p.m.ProcessRecovery(ctx, p.b)
				if err != nil || state.Status != "blocked" {
					t.Fatalf("unsupported history accepted %+v %v", state, err)
				}
			}
			cases, err := p.m.RecoveryCases(ctx, p.b.NodeID)
			if err != nil || len(cases) != 1 {
				t.Fatalf("history case absent %v", err)
			}
			got, err := p.m.Get(ctx, op.OperationID)
			if err != nil || !got.Blocked || got.LatestResultSeq != 0 {
				t.Fatalf("unsupported history projected a terminal %+v %v", got, err)
			}
		})
	}
}

func TestRecoveryRootReleasedWrongDigestNeverAcknowledges(t *testing.T) {
	p := riOpenPair(t)
	ctx := context.Background()
	op, _ := riPrepare(t, p, 928, false)
	riScan(t, p, "all", 10)
	riComplete(t, p)
	op.Digest = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if ack, err := p.m.ReceiveRecoveryEvent(ctx, p.b, eventResult(t, p.m, p.b, op, "not_started", 0, "", "", "canceled_before_start")); !errors.Is(err, ErrConflict) || len(ack) != 0 {
		t.Fatalf("wrong digest acknowledged %v", err)
	}
	riFrozen(t, p.m, p.b.NodeID, true)
}

func TestRecoveryRootAgentBlockedResultCannotCreateFinalize(t *testing.T) {
	p := riOpenPair(t)
	ctx := context.Background()
	op, _ := riPrepare(t, p, 923, true)
	starts := 0
	op = riAuthorize(t, p, op, &starts)
	raw := eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", "")
	r := rootRecordFor(op)
	r["phase"] = "reconciliation_required"
	r["last_event_seq"] = 2
	r["result_event_seq"] = 2
	r["latest_result"] = json.RawMessage(mfEnvelope(t, raw).Body)
	rootReceiveRecord(t, p, r)
	state, err := p.m.ProcessRecovery(ctx, p.b)
	if err != nil || state.Status != "blocked" {
		t.Fatalf("blocked Agent result projected success %+v %v", state, err)
	}
	op, err = p.m.Get(ctx, op.OperationID)
	if err != nil || !op.Blocked || op.LatestResultSeq != 0 {
		t.Fatalf("case changed result %+v %v", op, err)
	}
	intents, err := p.m.Intents(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	for _, intent := range intents {
		if intent.Kind == "finalize" && intent.State != "suppressed" {
			t.Fatal("created finalize from isolated page")
		}
	}
	cases, err := p.m.RecoveryCases(ctx, p.b.NodeID)
	if err != nil || len(cases) != 1 {
		t.Fatalf("missing case %v", err)
	}
	body, err := p.m.ReadRecoveryCase(ctx, cases[0].ID)
	if err != nil || !bytes.Contains(body, mfEnvelope(t, raw).Body) {
		t.Fatalf("lost result evidence %v", err)
	}
}

func TestRecoveryRootIsolatedPageBlocksResultBeforeProcess(t *testing.T) {
	for _, phase := range []string{"reconciliation_required", "finalized"} {
		t.Run(phase, func(t *testing.T) {
			p := riOpenPair(t)
			ctx := context.Background()
			op, _ := riPrepare(t, p, 931, true)
			starts := 0
			op = riAuthorize(t, p, op, &starts)
			raw := eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", "")
			r := rootRecordFor(op)
			r["phase"] = phase
			r["last_event_seq"] = 2
			r["result_event_seq"] = 2
			r["latest_result"] = json.RawMessage(mfEnvelope(t, raw).Body)
			if phase == "finalized" {
				r["claims_held"] = false
				r["final_state"] = "succeeded"
				r["finalized_result_event_seq"] = 2
			}
			rootReceiveRecord(t, p, r)
			if ack, err := p.m.ReceiveRecoveryEvent(ctx, p.b, raw); !errors.Is(err, ErrConflict) || len(ack) != 0 {
				t.Fatalf("isolated page allowed terminal event ack=%d err=%v", len(ack), err)
			}
			got, err := p.m.Get(ctx, op.OperationID)
			if err != nil || !got.Blocked || got.LatestResultSeq != 0 {
				t.Fatalf("isolated page produced success %+v %v", got, err)
			}
		})
	}
}

func TestRecoveryRootAbsenceProofMustPostdateAcceptance(t *testing.T) {
	p := riOpenPair(t)
	ctx := context.Background()
	old, err := p.m.ReadRecoveryState(ctx, p.b.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := riPrepare(t, p, 932, false)
	riScan(t, p, "all", 10)
	riComplete(t, p)
	if _, err = p.m.db.Exec(`UPDATE absence_releases SET round_id=? WHERE operation_id=?`, old.RoundID, op.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err = p.m.Get(ctx, op.OperationID); !errors.Is(err, ErrEvidence) {
		t.Fatalf("pre-acceptance empty scan accepted as release proof %v", err)
	}
}

func TestRecoveryRootAuthorizedMissingHistoryKeepsClaims(t *testing.T) {
	p := riOpenPair(t)
	ctx := context.Background()
	op, _ := riPrepare(t, p, 933, false)
	if err := p.m.RecordReadyFixture(ctx, p.b, op.OperationID, op.Digest); err != nil {
		t.Fatal(err)
	}
	request, _ := protocol.NewMessage("request_start", p.b.SessionID, "", map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "start_nonce": riID(9935)})
	if _, err := p.m.AuthorizeStart(ctx, p.b, request); err != nil {
		t.Fatal(err)
	}
	riScan(t, p, "all", 10)
	state, err := p.m.ProcessRecovery(ctx, p.b)
	if err != nil || state.Status != "blocked" {
		t.Fatalf("authorized absence treated as cancellation %+v %v", state, err)
	}
	got, err := p.m.Get(ctx, op.OperationID)
	if err != nil || got.PermitID == "" || got.ReleaseStatus != "held" || !got.Blocked {
		t.Fatalf("lost authorization %+v %v", got, err)
	}
	claims, err := p.m.Claims(ctx, op.OperationID)
	if err != nil || len(claims) != 2 {
		t.Fatalf("released authorized unknown %v", err)
	}
	if _, err = p.m.PrepareRecoveryComplete(ctx, p.b); err == nil {
		t.Fatal("authorized unknown completed")
	}
}

func TestRecoveryRootUnknownAndMismatchedRecordsNeverFabricateOperations(t *testing.T) {
	for _, kind := range []string{"unknown", "wrong-target"} {
		t.Run(kind, func(t *testing.T) {
			p := riOpenPair(t)
			ctx := context.Background()
			op, _ := riPrepare(t, p, 934, false)
			r := rootRecordFor(op)
			if kind == "unknown" {
				r["operation_id"] = riID(9936)
			} else {
				r["stack_id"] = riID(9937)
			}
			rootReceiveRecord(t, p, r)
			state, err := p.m.ProcessRecovery(ctx, p.b)
			if err != nil || state.Status != "blocked" {
				t.Fatalf("bad record accepted %+v %v", state, err)
			}
			if mfCount(t, p.m, "operations") != 1 {
				t.Fatal("fabricated unknown operation")
			}
			cases, err := p.m.RecoveryCases(ctx, p.b.NodeID)
			if err != nil || len(cases) != 1 {
				t.Fatalf("missing case %v", err)
			}
			claims, err := p.m.Claims(ctx, op.OperationID)
			if err != nil || len(claims) != 2 {
				t.Fatalf("lost original claims %v", err)
			}
		})
	}
}

func TestRecoveryRootV2RejectsWrongIdentityKeyAndDDL(t *testing.T) {
	for _, change := range []string{"identity", "key", "ddl"} {
		t.Run(change, func(t *testing.T) {
			s, _, path := recoveryStoreFixture(t)
			expected := s.Identity()
			keys := storageKeys()
			want := ErrSchema
			switch change {
			case "identity":
				expected.ManagerID = riID(9938)
				want = ErrIdentity
			case "key":
				keys.DigestKey[0] ^= 1
				want = ErrKey
			case "ddl":
				if _, err := s.db.Exec(`CREATE TABLE extra(value TEXT)`); err != nil {
					t.Fatal(err)
				}
			}
			got, err := OpenRecovery(context.Background(), path, expected, keys)
			if got != nil || !errors.Is(err, want) {
				t.Fatalf("wrong %s accepted %v", change, err)
			}
		})
	}
}

func TestRecoveryRootRecordCrossFields(t *testing.T) {
	for _, change := range []string{"valid", "watermark", "ack-without-result", "released-without-finalized", "final-without-result"} {
		t.Run(change, func(t *testing.T) {
			m := recoveryRootRecord()
			switch change {
			case "watermark":
				m["last_event_seq"] = -1
			case "ack-without-result":
				m["report_acked"] = true
			case "released-without-finalized":
				m["claims_held"] = false
			case "final-without-result":
				m["final_state"] = "canceled"
			}
			raw, _ := json.Marshal(m)
			_, err := parseRecoveryRecord(raw)
			if (err == nil) != (change == "valid") {
				t.Fatalf("%s accepted=%v", change, err)
			}
		})
	}
}
