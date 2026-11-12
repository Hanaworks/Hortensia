package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"docker-manager/internal/protocol"
)

func releaseFixture(t *testing.T) (*Store, Binding, Operation, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "release.sqlite")
	s, err := CreateRelease(ctx, path, eventManager, eventKeys())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b, raw, _ := eventPrepare(t, s)
	if err = s.BindRecoverySessionFixture(ctx, b, recoveryAgentBoot); err != nil {
		t.Fatal(err)
	}
	// Controlled admission fixture, independently of recovery handshake tests.
	if _, err = s.db.Exec(`UPDATE nodes SET frozen=0`); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateUserFixture(ctx, eventUser, true); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"node.read", "config.read", "stack.deploy"} {
		if err = s.SetGrantFixture(ctx, eventUser, b.NodeID, c, true); err != nil {
			t.Fatal(err)
		}
	}
	op, err := s.AcceptFixture(ctx, b, eventUser, raw)
	if err != nil {
		t.Fatal(err)
	}
	return s, b, op, path
}
func releaseReceipt(t *testing.T, s *Store, request []byte) []byte {
	t.Helper()
	e, err := s.validator.Validate(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := protocol.NewMessage("finalized", *e.SessionID, e.MessageID, e.Body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestReleaseManagerPersistsReceiptAndStableRequest(t *testing.T) {
	ctx := context.Background()
	s, b, op, path := releaseFixture(t)
	raw := eventResult(t, s, b, op, "not_started", 0, "", "", "canceled_before_start")
	if _, err := s.ReceiveEvent(ctx, b, raw); err != nil {
		t.Fatal(err)
	}
	request, err := s.PrepareFinalize(ctx, b, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.PrepareFinalize(ctx, b, op.OperationID)
	if err != nil || !bytes.Equal(request, again) {
		t.Fatalf("unstable request %v", err)
	}
	receipt := releaseReceipt(t, s, request)
	if err = s.ReceiveFinalized(ctx, b, receipt); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(ctx, op.OperationID)
	if err != nil || before.ReleaseStatus != "released" || before.State != "canceled" {
		t.Fatalf("released %+v %v", before, err)
	}
	claims, err := s.Claims(ctx, op.OperationID)
	if err != nil || len(claims) != 0 {
		t.Fatalf("claims %+v %v", claims, err)
	}
	body, err := s.ReadFinalized(ctx, op.OperationID)
	if err != nil || len(body) == 0 {
		t.Fatalf("receipt missing %v", err)
	}
	if _, err = s.db.Exec(`UPDATE keys SET uses=1048576`); err != nil {
		t.Fatal(err)
	}
	if err = s.ReceiveFinalized(ctx, b, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReceiveEvent(ctx, b, raw); err != nil {
		t.Fatalf("same event replay %v", err)
	}
	after, err := s.Get(ctx, op.OperationID)
	if err != nil || after.Revision != before.Revision {
		t.Fatalf("replay rewrote %+v %v", after, err)
	}
	if other, err := OpenRecovery(ctx, path, s.Identity(), eventKeys()); !errors.Is(err, ErrSchema) || other != nil {
		t.Fatalf("v2 accepted v3 %v", err)
	}
	reopened, err := OpenRelease(ctx, path, s.Identity(), eventKeys())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := reopened.ReadFinalized(ctx, op.OperationID)
	if err != nil || !bytes.Equal(body, recovered) {
		t.Fatalf("receipt reopen %v", err)
	}
}
func TestReleaseManagerRejectsWrongReplyWithoutRelease(t *testing.T) {
	ctx := context.Background()
	s, b, op, _ := releaseFixture(t)
	raw := eventResult(t, s, b, op, "not_started", 0, "", "", "fixture_failure")
	if _, err := s.ReceiveEvent(ctx, b, raw); err != nil {
		t.Fatal(err)
	}
	request, err := s.PrepareFinalize(ctx, b, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	receipt := releaseReceipt(t, s, request)
	env, _ := s.validator.Validate(receipt)
	wrong, _ := protocol.NewMessage("finalized", b.SessionID, validationID, env.Body)
	if err = s.ReceiveFinalized(ctx, b, wrong); err == nil {
		t.Fatal("wrong reply accepted")
	}
	claims, err := s.Claims(ctx, op.OperationID)
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims %+v %v", claims, err)
	}
}

func releasePending(t *testing.T) (*Store, Binding, Operation, []byte, []byte) {
	t.Helper()
	s, b, op, _ := releaseFixture(t)
	raw := eventResult(t, s, b, op, "not_started", 0, "", "", "canceled_before_start")
	if _, err := s.ReceiveEvent(context.Background(), b, raw); err != nil {
		t.Fatal(err)
	}
	request, err := s.PrepareFinalize(context.Background(), b, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	return s, b, op, request, releaseReceipt(t, s, request)
}
func TestReleaseManagerFaultStatementsAndCommitAtomic(t *testing.T) {
	for _, clause := range []string{
		"AFTER INSERT ON main.finalization",
		"AFTER UPDATE OF release_status ON main.operations WHEN NEW.release_status='released'",
		"AFTER UPDATE OF delivery_state ON main.outbox WHEN NEW.kind='finalize'",
		"AFTER DELETE ON main.resource_claims",
		"AFTER UPDATE OF accepted ON main.finalize_requests",
	} {
		t.Run(clause, func(t *testing.T) {
			for _, commit := range []bool{false, true} {
				t.Run(fmt.Sprint(commit), func(t *testing.T) {
					s, b, op, _, receipt := releasePending(t)
					ctx := context.Background()
					before, err := s.Get(ctx, op.OperationID)
					if err != nil {
						t.Fatal(err)
					}
					uses := mfUses(t, s)
					action := "SELECT RAISE(ABORT,'release-test-failure');"
					if commit {
						if _, err = s.db.Exec(`CREATE TEMP TABLE release_parent(id TEXT PRIMARY KEY); CREATE TEMP TABLE release_child(parent TEXT REFERENCES release_parent(id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
							t.Fatal(err)
						}
						action = "INSERT INTO release_child(parent) VALUES('missing');"
					}
					if _, err = s.db.Exec("CREATE TEMP TRIGGER release_fault " + clause + " BEGIN " + action + " END"); err != nil {
						t.Fatal(err)
					}
					if err = s.ReceiveFinalized(ctx, b, receipt); !errors.Is(err, ErrStorage) {
						t.Fatalf("failure %v", err)
					}
					after, err := s.Get(ctx, op.OperationID)
					if err != nil || after != before {
						t.Fatalf("partial operation before=%+v after=%+v err=%v", before, after, err)
					}
					claims, err := s.Claims(ctx, op.OperationID)
					if err != nil || len(claims) != 2 {
						t.Fatalf("partial claims %+v %v", claims, err)
					}
					if mfCount(t, s, "finalization") != 0 || mfUses(t, s) != uses+1 {
						t.Fatal("partial receipt or reservation refunded")
					}
					var accepted int
					if err = s.db.QueryRow(`SELECT accepted FROM finalize_requests WHERE operation_id=?`, op.OperationID).Scan(&accepted); err != nil || accepted != 0 {
						t.Fatalf("request accepted %d %v", accepted, err)
					}
					if commit && mfCount(t, s, "temp.release_child") != 0 {
						t.Fatal("failed commit left child")
					}
					if _, err = s.db.Exec(`DROP TRIGGER release_fault`); err != nil {
						t.Fatal(err)
					}
					if err = s.ReceiveFinalized(ctx, b, receipt); err != nil {
						t.Fatalf("retry %v", err)
					}
					if claims, err = s.Claims(ctx, op.OperationID); err != nil || len(claims) != 0 {
						t.Fatalf("retry claims %+v %v", claims, err)
					}
				})
			}
		})
	}
}
func TestReleaseManagerOldBindingDoesNotQuarantineCurrent(t *testing.T) {
	for _, change := range []string{"session", "agent_boot", "manager_boot"} {
		t.Run(change, func(t *testing.T) {
			s, b, op, oldRequest, oldReceipt := releasePending(t)
			ctx := context.Background()
			if change == "session" {
				b.SessionID = eventSession2
			}
			agent := recoveryAgentBoot
			if change == "agent_boot" {
				agent = riID(9997)
			}
			if change == "manager_boot" { // Open the same real store while preserving audit rows.
				var dbPath string
				if err := s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dbPath); err != nil {
					t.Fatal(err)
				}
				reopened, err := OpenRelease(ctx, dbPath, s.Identity(), eventKeys())
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				s = reopened
			}
			if err := s.BindRecoverySessionFixture(ctx, b, agent); err != nil {
				t.Fatal(err)
			}
			if err := s.ReceiveFinalized(ctx, b, oldReceipt); err == nil {
				t.Fatal("obsolete receipt accepted")
			}
			if mfCount(t, s, "recovery_cases") != 0 {
				t.Fatal("old reply quarantined current")
			}
			current, err := s.PrepareFinalize(ctx, b, op.OperationID)
			if err != nil || bytes.Equal(current, oldRequest) {
				t.Fatalf("new binding request %v", err)
			}
			if err = s.ReceiveFinalized(ctx, b, releaseReceipt(t, s, current)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestReleaseManagerConflictingReceiptAndEventPreserveHistory(t *testing.T) {
	for _, conflict := range []string{"receipt", "event"} {
		t.Run(conflict, func(t *testing.T) {
			s, b, op, request, receipt := releasePending(t)
			ctx := context.Background()
			if err := s.ReceiveFinalized(ctx, b, receipt); err != nil {
				t.Fatal(err)
			}
			before, err := s.Get(ctx, op.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := s.ReadFinalized(ctx, op.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if conflict == "receipt" {
				env, _ := s.validator.Validate(request)
				body, _ := parseFinalization(env.Body)
				body.State = "failed"
				wrong, _ := protocol.NewMessage("finalized", b.SessionID, env.MessageID, body)
				err = s.ReceiveFinalized(ctx, b, wrong)
			} else {
				wrong := eventResult(t, s, b, op, "not_started", 0, "", "", "different_error")
				_, err = s.ReceiveEvent(ctx, b, wrong)
			}
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("conflict %v", err)
			}
			after, err := s.Get(ctx, op.OperationID)
			if err != nil || after != before {
				t.Fatalf("conflict rewrote history %+v %v", after, err)
			}
			again, err := s.ReadFinalized(ctx, op.OperationID)
			if err != nil || !bytes.Equal(saved, again) {
				t.Fatalf("changed receipt %v", err)
			}
			claims, err := s.Claims(ctx, op.OperationID)
			if err != nil || len(claims) != 0 {
				t.Fatalf("resurrected claims %+v %v", claims, err)
			}
			blocked, err := s.recoveryQuarantined(ctx, s.db, b.NodeID)
			if err != nil || !blocked || mfCount(t, s, "recovery_cases") != 1 {
				t.Fatalf("not isolated %v", err)
			}
			if _, err = s.PrepareFinalize(ctx, b, op.OperationID); !errors.Is(err, ErrFrozen) {
				t.Fatalf("isolated finalize %v", err)
			}
		})
	}
}
func TestReleaseManagerReceiptTamperingRejected(t *testing.T) {
	for _, change := range []string{
		`UPDATE finalization SET body_cipher=zeroblob(length(body_cipher))`,
		`UPDATE finalization SET terminal_state='failed'`,
		`UPDATE finalize_requests SET accepted=0`,
		`UPDATE finalize_requests SET manager_boot='00000000-0000-4000-8000-000000000099'`,
		`UPDATE finalize_requests SET raw=CAST(replace(CAST(raw AS TEXT),'canceled','failed') AS BLOB)`,
	} {
		t.Run(change, func(t *testing.T) {
			s, b, op, _, receipt := releasePending(t)
			ctx := context.Background()
			if err := s.ReceiveFinalized(ctx, b, receipt); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(change); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReadFinalized(ctx, op.OperationID); !errors.Is(err, ErrEvidence) {
				t.Fatalf("tampered read %v", err)
			}
			if _, err := s.Claims(ctx, op.OperationID); !errors.Is(err, ErrEvidence) {
				t.Fatalf("tampered claims %v", err)
			}
		})
	}
}

func TestReleaseManagerMissingHistoryDependsOnScope(t *testing.T) {
	for _, scope := range []string{"all", "pending"} {
		t.Run(scope, func(t *testing.T) {
			ctx := context.Background()
			s, b, op, _, receipt := releasePending(t)
			if err := s.ReceiveFinalized(ctx, b, receipt); err != nil {
				t.Fatal(err)
			}
			request, err := s.StartRecovery(ctx, b, scope, 10)
			if err != nil {
				t.Fatal(err)
			}
			page := recoveryPageReply(t, s, b, request, 0, []any{}, nil)
			if _, err = s.ReceiveRecoveryPage(ctx, b, page); err != nil {
				t.Fatal(err)
			}
			state, err := s.ProcessRecovery(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			want := "blocked"
			if scope == "pending" {
				want = "ready"
			}
			if state.Status != want {
				t.Fatalf("missing %s state %+v", scope, state)
			}
			cases, err := s.RecoveryCases(ctx, b.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			if scope == "all" && (len(cases) != 1 || cases[0].Kind != "missing_history") {
				t.Fatalf("all missing %+v", cases)
			}
			if scope == "pending" && len(cases) != 0 {
				t.Fatalf("pending missing %+v", cases)
			}
			if _, err = s.ReadFinalized(ctx, op.OperationID); err != nil {
				t.Fatalf("history lost %v", err)
			}
			if _, err = s.PrepareRecoveryComplete(ctx, b); (err == nil) != (scope == "pending") {
				t.Fatalf("complete barrier %s %v", scope, err)
			}
		})
	}
}
func TestReleaseManagerSupportsFinalizedPageWithoutFabricatingReceipt(t *testing.T) {
	ctx := context.Background()
	s, b, op, _ := releaseFixture(t)
	result := eventResult(t, s, b, op, "not_started", 0, "", "", "canceled_before_start")
	env, _ := s.validator.Validate(result)
	record := rootRecordFor(op)
	record["phase"] = "finalized"
	record["claims_held"] = false
	record["last_event_seq"] = 1
	record["result_event_seq"] = 1
	record["latest_result"] = env.Body
	record["final_state"] = "canceled"
	record["finalized_result_event_seq"] = 1
	request, err := s.StartRecovery(ctx, b, "all", 10)
	if err != nil {
		t.Fatal(err)
	}
	page := recoveryPageReply(t, s, b, request, 1, []any{record}, nil)
	if _, err = s.ReceiveRecoveryPage(ctx, b, page); err != nil {
		t.Fatal(err)
	}
	state, err := s.ProcessRecovery(ctx, b)
	if err != nil || state.Status != "waiting" {
		t.Fatalf("process %+v %v", state, err)
	}
	claims, err := s.Claims(ctx, op.OperationID)
	if err != nil || len(claims) != 2 || mfCount(t, s, "finalization") != 0 {
		t.Fatalf("page fabricated release %+v %v", claims, err)
	}
	request, err = s.PrepareFinalize(ctx, b, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReceiveFinalized(ctx, b, releaseReceipt(t, s, request)); err != nil {
		t.Fatal(err)
	}
	state, err = s.ProcessRecovery(ctx, b)
	if err != nil || state.Status != "ready" {
		t.Fatalf("confirmed page %+v %v", state, err)
	}
}
func TestReleaseManagerChecksEveryCurrentRoundAndLateContradiction(t *testing.T) {
	ctx := context.Background()
	s, b, op, _, receipt := releasePending(t)
	if err := s.ReceiveFinalized(ctx, b, receipt); err != nil {
		t.Fatal(err)
	}
	// A frozen page from before release is compatible, but no old accepted
	// request can substitute for this new round's confirmation.
	record := rootRecordFor(op)
	request, err := s.StartRecovery(ctx, b, "all", 10)
	if err != nil {
		t.Fatal(err)
	}
	page := recoveryPageReply(t, s, b, request, 1, []any{record}, nil)
	if _, err = s.ReceiveRecoveryPage(ctx, b, page); err != nil {
		t.Fatal(err)
	}
	state, err := s.ProcessRecovery(ctx, b)
	if err != nil || state.Status != "waiting" {
		t.Fatalf("old request unlocked %+v %v", state, err)
	}
	if _, err = s.PrepareRecoveryComplete(ctx, b); !errors.Is(err, ErrRecoveryIncomplete) {
		t.Fatalf("no current confirmation %v", err)
	}
	request, err = s.PrepareFinalize(ctx, b, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReceiveFinalized(ctx, b, releaseReceipt(t, s, request)); err != nil {
		t.Fatal(err)
	}
	state, err = s.ProcessRecovery(ctx, b)
	if err != nil || state.Status != "ready" {
		t.Fatalf("current confirmation %+v %v", state, err)
	}
	complete, err := s.PrepareRecoveryComplete(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	wrong := eventResult(t, s, b, op, "not_started", 0, "", "", "different_error")
	if _, err = s.ReceiveEvent(ctx, b, wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict %v", err)
	}
	completeEnv, _ := s.validator.Validate(complete)
	completed, err := protocol.NewMessage("reconcile_completed", b.SessionID, completeEnv.MessageID, completeEnv.Body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReceiveRecoveryCompleted(ctx, b, completed); !errors.Is(err, ErrRecoveryIncomplete) {
		t.Fatalf("late contradiction did not block completed: %v", err)
	}
}

func TestReleaseManagerEarlyPreparationDoesNotQuarantineOperation(t *testing.T) {
	ctx := context.Background()
	s, b, op, _ := releaseFixture(t)
	if _, err := s.PrepareFinalize(ctx, b, op.OperationID); err == nil {
		t.Fatal("missing result finalized")
	}
	got, err := s.Get(ctx, op.OperationID)
	if err != nil || got != op || mfCount(t, s, "recovery_cases") != 0 {
		t.Fatalf("local premature call changed operation %+v %v", got, err)
	}
	raw := eventResult(t, s, b, op, "not_started", 0, "", "", "canceled_before_start")
	if _, err = s.ReceiveEvent(ctx, b, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareFinalize(ctx, b, op.OperationID); err != nil {
		t.Fatalf("eventual result cannot finalize %v", err)
	}
}
func TestReleaseManagerCurrentUnsupportedFinalizedSequenceIsConflict(t *testing.T) {
	ctx := context.Background()
	s, b, op, request, _ := releasePending(t)
	env, _ := s.validator.Validate(request)
	body, _ := parseFinalization(env.Body)
	body.ResultSeq = 3
	wrong, err := protocol.NewMessage("finalized", b.SessionID, env.MessageID, body)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReceiveFinalized(ctx, b, wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("version conflict %v", err)
	}
	claims, err := s.Claims(ctx, op.OperationID)
	if err != nil || len(claims) != 2 || mfCount(t, s, "recovery_cases") != 1 {
		t.Fatalf("conflict lost claims/case %+v %v", claims, err)
	}
}

func TestReleaseManagerTerminalMatrixPreservesUnresolvedResults(t *testing.T) {
	cases := []struct {
		name, outcome, observation, ownership, code, want string
		exit                                              int
	}{
		{"canceled", "not_started", "", "", "canceled_before_start", "canceled", 0},
		{"not_started_failed", "not_started", "", "", "fixture_failure", "failed", 0},
		{"zero", "normal_exit", "complete", "consistent", "", "succeeded", 0},
		{"nonzero", "normal_exit", "complete", "consistent", "fixture_failure", "failed", 7},
		{"unknown", "unknown", "", "unknown", "", "", 0},
		{"interrupted", "interrupted", "", "unknown", "", "", 0},
		{"incomplete", "normal_exit", "error", "consistent", "", "", 0},
		{"ownership", "normal_exit", "complete", "drifted", "", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s, b, op, _ := releaseFixture(t)
			if c.outcome != "not_started" {
				if err := s.RecordReadyFixture(ctx, b, op.OperationID, op.Digest); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AuthorizeStart(ctx, b, mfRequest(t, b, op)); err != nil {
					t.Fatal(err)
				}
				var err error
				op, err = s.Get(ctx, op.OperationID)
				if err != nil {
					t.Fatal(err)
				}
			}
			raw := eventResult(t, s, b, op, c.outcome, c.exit, c.observation, c.ownership, c.code)
			if _, err := s.ReceiveEvent(ctx, b, raw); err != nil {
				t.Fatal(err)
			}
			before, err := s.Get(ctx, op.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			request, err := s.PrepareFinalize(ctx, b, op.OperationID)
			if c.want == "" {
				if !errors.Is(err, ErrRecoveryIncomplete) {
					t.Fatalf("unresolved result %v", err)
				}
				after, err := s.Get(ctx, op.OperationID)
				if err != nil || after != before || mfCount(t, s, "recovery_cases") != 0 {
					t.Fatalf("local incomplete added conflict %+v %v", after, err)
				}
				claims, err := s.Claims(ctx, op.OperationID)
				if err != nil || len(claims) != 2 {
					t.Fatalf("unresolved claims %+v %v", claims, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			env, _ := s.validator.Validate(request)
			body, _ := parseFinalization(env.Body)
			if body.State != c.want {
				t.Fatalf("terminal=%s want=%s", body.State, c.want)
			}
			if err = s.ReceiveFinalized(ctx, b, releaseReceipt(t, s, request)); err != nil {
				t.Fatal(err)
			}
			after, err := s.Get(ctx, op.OperationID)
			if err != nil || after.State != c.want || after.ReleaseStatus != "released" {
				t.Fatalf("release result %+v %v", after, err)
			}
		})
	}
}
func TestReleaseManagerExplicitVersionsAndFreeze(t *testing.T) {
	ctx := context.Background()
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "version.sqlite")
			s, err := createVersion(ctx, path, storageManagerID, storageKeys(), version)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for _, want := range []int{1, 2, 3} {
				if want == version {
					continue
				}
				opened, err := openVersion(ctx, path, s.Identity(), storageKeys(), want)
				if opened != nil || !errors.Is(err, ErrSchema) {
					t.Fatalf("opened v%d as v%d: %v", version, want, err)
				}
			}
			if version != 3 {
				if _, err = s.PrepareFinalize(ctx, Binding{}, validationID); !errors.Is(err, ErrSchema) {
					t.Fatalf("legacy prepare release %v", err)
				}
				return
			}
			b := Binding{NodeID: riID(6001), LedgerID: riID(6002), SessionID: riID(6003)}
			if err = s.BindRecoverySessionFixture(ctx, b, recoveryAgentBoot); err != nil {
				t.Fatal(err)
			}
			if err = s.requireBinding(ctx, s.db, b, false); !errors.Is(err, ErrFrozen) {
				t.Fatalf("new node not frozen %v", err)
			}
			opened, err := OpenRelease(ctx, path, s.Identity(), storageKeys())
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			if err = opened.BindRecoverySessionFixture(ctx, b, recoveryAgentBoot); err != nil {
				t.Fatal(err)
			}
			if err = opened.requireBinding(ctx, opened.db, b, false); !errors.Is(err, ErrFrozen) {
				t.Fatalf("reopen not frozen %v", err)
			}
		})
	}
	for _, change := range []string{"identity", "key", "ddl"} {
		t.Run(change, func(t *testing.T) {
			s, _, _, path := releaseFixture(t)
			id, keys := s.Identity(), eventKeys()
			want := ErrSchema
			switch change {
			case "identity":
				id.ManagerID = riID(6010)
				want = ErrIdentity
			case "key":
				keys.EncryptionKey[0] ^= 1
				want = ErrKey
			case "ddl":
				if _, err := s.db.Exec(`CREATE TABLE unexpected(x TEXT)`); err != nil {
					t.Fatal(err)
				}
			}
			opened, err := OpenRelease(ctx, path, id, keys)
			if opened != nil || !errors.Is(err, want) {
				t.Fatalf("bad %s opened: %v", change, err)
			}
		})
	}
}
