package ledger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCryptoJournal(t *testing.T) (*Store, string, []byte, JournalKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	key := bytes.Repeat([]byte{0x42}, 32)
	jk := JournalKey{ID: "journal-key-1", Key: bytes.Repeat([]byte{0x73}, 32)}
	s, err := CreateJournal(context.Background(), path, testNode, "key-1", key, jk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, path, key, jk
}

func TestJournalCreateOpenAndKeyPairing(t *testing.T) {
	s, path, key, jk := newCryptoJournal(t)
	ctx, identity, boot := context.Background(), s.Identity(), s.boot
	assertConnectionSettings(t, s.db)
	s.db.SetMaxIdleConns(0)
	assertConnectionSettings(t, s.db)
	s.db.SetMaxIdleConns(1)
	var check []byte
	var uses int64
	if err := s.db.QueryRow(`SELECT key_check,uses FROM event_keys WHERE id=1`).Scan(&check, &uses); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]string{identity.NodeID, identity.LedgerID, jk.ID})
	mac := hmac.New(sha256.New, jk.Key)
	mac.Write([]byte("dm-event-key-v1\x00"))
	mac.Write(raw)
	if !hmac.Equal(check, mac.Sum(nil)) || uses != 0 {
		t.Fatal("empty journal key binding or initial count incorrect")
	}
	for _, bad := range []JournalKey{{ID: "wrong", Key: jk.Key}, {ID: jk.ID, Key: bytes.Repeat([]byte{0x21}, 32)}, {ID: jk.ID}, {ID: jk.ID, Key: key}} {
		got, err := OpenJournal(ctx, path, identity, key, bad)
		if got != nil {
			got.Close()
		}
		if !errors.Is(err, ErrKey) {
			t.Fatalf("empty store accepted wrong journal key: %v", err)
		}
	}
	if _, err := Open(ctx, path, identity, key); !errors.Is(err, ErrSchema) {
		t.Fatalf("v1 accepted v2: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenJournal(ctx, path, identity, key, jk)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if opened.Identity() != identity || opened.boot == boot {
		t.Fatal("identity changed or boot reused")
	}
	v1, p1, k1 := newTestStore(t)
	if _, err := OpenJournal(ctx, p1, v1.Identity(), k1, jk); !errors.Is(err, ErrSchema) {
		t.Fatalf("v2 accepted v1: %v", err)
	}
	if err := v1.requireJournal(); !errors.Is(err, ErrJournal) {
		t.Fatalf("v1 journal method: %v", err)
	}
}

func TestJournalInvalidCreationDoesNotCreateFile(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	for _, jk := range []JournalKey{{}, {ID: "ok", Key: key}, {ID: "", Key: make([]byte, 32)}, {ID: strings.Repeat("x", 129), Key: make([]byte, 32)}, {ID: string([]byte{0xff}), Key: make([]byte, 32)}, {ID: "ok", Key: make([]byte, 31)}} {
		path := filepath.Join(t.TempDir(), "bad.sqlite")
		if got, err := CreateJournal(context.Background(), path, testNode, "key-1", key, jk); !errors.Is(err, ErrKey) {
			if got != nil {
				got.Close()
			}
			t.Fatalf("invalid key accepted: %v", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid key created a file")
		}
	}
}

func TestJournalRejectsAlteredSchemaAndSupportsLineEndings(t *testing.T) {
	for _, mutation := range []string{`CREATE TABLE extra(value TEXT)`, `DROP TABLE event_conflicts`, `PRAGMA user_version=1`, `ALTER TABLE event_conflicts RENAME TO old_conflicts; CREATE TABLE event_conflicts AS SELECT * FROM old_conflicts; DROP TABLE old_conflicts;`} {
		t.Run(mutation, func(t *testing.T) {
			s, path, key, jk := newCryptoJournal(t)
			id := s.Identity()
			if _, err := s.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			s.Close()
			got, err := OpenJournal(context.Background(), path, id, key, jk)
			if got != nil {
				got.Close()
			}
			if !errors.Is(err, ErrSchema) {
				t.Fatalf("schema accepted: %v", err)
			}
		})
	}
	for _, expression := range []string{`replace(sql,char(13)||char(10),char(10))`, `replace(replace(sql,char(13)||char(10),char(10)),char(10),char(13)||char(10))`} {
		s, path, key, jk := newCryptoJournal(t)
		id := s.Identity()
		if _, err := s.db.Exec(`PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=` + expression + ` WHERE name NOT GLOB 'sqlite_*'; PRAGMA writable_schema=OFF;`); err != nil {
			t.Fatal(err)
		}
		s.Close()
		opened, err := OpenJournal(context.Background(), path, id, key, jk)
		if err != nil {
			t.Fatal(err)
		}
		opened.Close()
	}
}

func TestJournalSchemaRejectsInvalidStateAndKeyFacts(t *testing.T) {
	s, _, _, _ := newCryptoJournal(t)
	r, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, prepareFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO event_state(operation_id,last_seq,blocked) VALUES(?,0,0) ON CONFLICT DO NOTHING`, r.OperationID); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`UPDATE event_keys SET id=2`, `UPDATE event_keys SET key_check=x'01'`, `UPDATE event_keys SET uses=-1`, `UPDATE event_keys SET uses=1048577`, `UPDATE event_keys SET uses=0.5`, `UPDATE event_keys SET key_id=''`, `INSERT INTO event_state VALUES('missing',0,NULL,0)`, `INSERT INTO event_conflicts VALUES('missing',1,zeroblob(32),0)`} {
		if _, err := s.db.Exec(stmt); err == nil {
			t.Fatalf("invalid durable fact accepted: %s", stmt)
		}
	}
	for _, stmt := range []string{`UPDATE event_state SET last_seq=-1`, `UPDATE event_state SET last_seq=9007199254740992`, `UPDATE event_state SET last_seq=0.5`, `UPDATE event_state SET latest_result_seq=1`, `UPDATE event_state SET last_seq=1,latest_result_seq=1`, `UPDATE event_state SET blocked=2`, `UPDATE event_state SET operation_id='missing'`} {
		if _, err := s.db.Exec(stmt); err == nil {
			t.Fatalf("invalid durable event state accepted: %s", stmt)
		}
	}
	e, err := s.sealEvent(context.Background(), eventBinding{OperationID: r.OperationID, Digest: r.Digest, Kind: "result", Seq: 1}, []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO execution_events(operation_id,event_seq,kind,body_cipher,wrapped_dek,key_id,body_fingerprint) VALUES(?,1,'result',?,?,?,?)`, r.OperationID, e.Cipher, e.WrappedDEK, e.KeyID, e.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE event_state SET last_seq=1,latest_result_seq=1`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`UPDATE execution_events SET event_seq=0`, `UPDATE execution_events SET event_seq=9007199254740992`, `UPDATE execution_events SET kind='other'`, `UPDATE execution_events SET body_cipher=x'01'`, `UPDATE execution_events SET wrapped_dek=x'01'`, `UPDATE execution_events SET body_fingerprint=x'01'`, `UPDATE execution_events SET key_id='missing'`, `DELETE FROM event_keys`, `DELETE FROM execution_events`, `DELETE FROM executions`} {
		if _, err := s.db.Exec(stmt); err == nil {
			t.Fatalf("invalid durable event accepted: %s", stmt)
		}
	}
}
