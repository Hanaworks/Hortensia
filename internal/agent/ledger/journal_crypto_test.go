package ledger

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func cryptoBinding() eventBinding {
	return eventBinding{OperationID: "00000000-0000-4000-8000-000000000004", Digest: "opaque-digest", Kind: "result", Seq: 2}
}

func TestJournalCryptoRoundTripUsesSpecifiedAADAndFingerprint(t *testing.T) {
	s, _, key, jk := newCryptoJournal(t)
	b, body := cryptoBinding(), []byte(" {\n\"secret\": \"private-result-9b65061a\",\"seq\": 2.0 }")
	sealed, err := s.sealEvent(context.Background(), b, body)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.sealEvent(context.Background(), b, body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sealed.Cipher, second.Cipher) || bytes.Equal(sealed.WrappedDEK, second.WrappedDEK) {
		t.Fatal("independent events reused ciphertext")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), sealed.Fingerprint) {
		t.Fatal("wrong original-body fingerprint")
	}
	aad := func(domain string) []byte {
		raw, _ := json.Marshal([]any{domain, s.identity.NodeID, s.identity.LedgerID, b.OperationID, b.Digest, b.Kind, b.Seq, jk.ID})
		return raw
	}
	block, _ := aes.NewCipher(jk.Key)
	wrap, _ := cipher.NewGCMWithRandomNonce(block)
	dek, err := wrap.Open(nil, nil, sealed.WrappedDEK, aad("dm-event-dek-v1"))
	if err != nil || len(dek) != 32 {
		t.Fatalf("wrong wrapping AAD: %v", err)
	}
	defer clear(dek)
	block, _ = aes.NewCipher(dek)
	data, _ := cipher.NewGCMWithRandomNonce(block)
	plain, err := data.Open(nil, nil, sealed.Cipher, aad("dm-event-body-v1"))
	if err != nil || !bytes.Equal(plain, body) {
		t.Fatalf("wrong body AAD: %v", err)
	}
	key[0] ^= 0xff
	jk.Key[0] ^= 0xff
	got, err := s.openEvent(b, sealed)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("caller key mutation affected store or body: %v", err)
	}
	got[0] ^= 1
	again, err := s.openEvent(b, sealed)
	if err != nil || !bytes.Equal(again, body) {
		t.Fatal("returned body aliases durable evidence")
	}
}

func TestJournalCryptoTamperingNeverReturnsBody(t *testing.T) {
	s, _, _, _ := newCryptoJournal(t)
	b := cryptoBinding()
	original, err := s.sealEvent(context.Background(), b, []byte("sensitive original body"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"cipher", "dek", "fingerprint", "key", "operation", "digest", "kind", "seq", "purpose"} {
		t.Run(field, func(t *testing.T) {
			e := sealedEvent{Cipher: bytes.Clone(original.Cipher), WrappedDEK: bytes.Clone(original.WrappedDEK), Fingerprint: bytes.Clone(original.Fingerprint), KeyID: original.KeyID}
			binding := b
			switch field {
			case "cipher":
				e.Cipher[0] ^= 1
			case "dek":
				e.WrappedDEK[0] ^= 1
			case "fingerprint":
				e.Fingerprint[0] ^= 1
			case "key":
				e.KeyID = "wrong"
			case "operation":
				binding.OperationID += "1"
			case "digest":
				binding.Digest += "1"
			case "kind":
				binding.Kind = "started"
			case "seq":
				binding.Seq++
			case "purpose":
				e.Cipher, e.WrappedDEK = e.WrappedDEK, e.Cipher
			}
			got, err := s.openEvent(binding, e)
			if !errors.Is(err, ErrEvidence) || len(got) != 0 {
				t.Fatalf("invalid evidence exposed body: %v", err)
			}
		})
	}
}

func TestJournalCryptoReservationSurvivesRollbackAndExhaustion(t *testing.T) {
	s, path, key, jk := newCryptoJournal(t)
	ctx, b := context.Background(), cryptoBinding()
	sealed, err := s.sealEvent(ctx, b, []byte("secret-reservation-body"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_keys SET uses=uses+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	var uses int64
	if err := s.db.QueryRow(`SELECT uses FROM event_keys WHERE id=1`).Scan(&uses); err != nil || uses != 1 {
		t.Fatalf("reservation rolled back: %d %v", uses, err)
	}
	if _, err := s.db.Exec(`UPDATE event_keys SET uses=? WHERE id=1`, maxKeyUses); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sealEvent(ctx, b, []byte("new body")); !errors.Is(err, ErrKeyExhausted) {
		t.Fatalf("exhaustion: %v", err)
	}
	id := s.Identity()
	s.Close()
	opened, err := OpenJournal(ctx, path, id, key, jk)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	got, err := opened.openEvent(b, sealed)
	if err != nil || string(got) != "secret-reservation-body" {
		t.Fatalf("exhausted key cannot read: %v", err)
	}
	if err := opened.db.QueryRow(`SELECT uses FROM event_keys WHERE id=1`).Scan(&uses); err != nil || uses != maxKeyUses {
		t.Fatalf("counter lost across open: %d %v", uses, err)
	}
}

func TestJournalCryptoReservationFailureDoesNotProduceCiphertext(t *testing.T) {
	for _, commitFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "statement", true: "commit"}[commitFailure], func(t *testing.T) {
			s, _, _, _ := newCryptoJournal(t)
			stmt := `CREATE TRIGGER fail_key_reservation BEFORE UPDATE ON event_keys BEGIN SELECT RAISE(ABORT,'private-trigger-secret'); END`
			if commitFailure {
				stmt = `CREATE TABLE key_fault(parent TEXT REFERENCES executions(operation_id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_key_reservation AFTER UPDATE ON event_keys BEGIN INSERT INTO key_fault(parent) VALUES('private-trigger-secret'); END`
			}
			if _, err := s.db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
			e, err := s.sealEvent(context.Background(), cryptoBinding(), []byte("body"))
			if !errors.Is(err, ErrStorage) || len(e.Cipher) > 0 || len(e.WrappedDEK) > 0 {
				t.Fatalf("failed reservation encrypted or exposed diagnostic: %v", err)
			}
			var uses int64
			if err := s.db.QueryRow(`SELECT uses FROM event_keys`).Scan(&uses); err != nil || uses != 0 {
				t.Fatalf("failed reservation consumed count: %d %v", uses, err)
			}
		})
	}
}

func TestJournalCryptoCloseClearsOwnedKeysAndDoesNotDeadlock(t *testing.T) {
	s, path, key, jk := newCryptoJournal(t)
	b := cryptoBinding()
	secret := []byte("close-private-body-b8738f760a")
	r, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, prepareFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	b.OperationID, b.Digest, b.Seq = r.OperationID, r.Digest, 1
	e, err := s.sealEvent(context.Background(), b, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO execution_events(operation_id,event_seq,kind,body_cipher,wrapped_dek,key_id,body_fingerprint) VALUES(?,?,?,?,?,?,?)`, b.OperationID, b.Seq, b.Kind, e.Cipher, e.WrappedDEK, e.KeyID, e.Fingerprint); err != nil {
		t.Fatal(err)
	}
	checkFiles := func() {
		t.Helper()
		for _, p := range []string{path, path + "-wal"} {
			raw, err := os.ReadFile(p)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, secret) || bytes.Contains(raw, key) || bytes.Contains(raw, jk.Key) {
				t.Fatal("secret written as plaintext")
			}
		}
	}
	checkFiles() // Inspect the live WAL before Close can checkpoint/remove it.
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.openEvent(b, e)
			if err != nil && !errors.Is(err, ErrStorage) {
				t.Errorf("close race: %v", err)
			}
		}()
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !bytes.Equal(s.key, make([]byte, 32)) || !bytes.Equal(s.journal.kek, make([]byte, 32)) {
		t.Fatal("owned key copies retained after Close")
	}
	if bytes.Equal(key, make([]byte, 32)) || bytes.Equal(jk.Key, make([]byte, 32)) {
		t.Fatal("Close mutated caller keys")
	}
	if err := s.requireJournal(); !errors.Is(err, ErrStorage) {
		t.Fatalf("closed journal accepted: %v", err)
	}
	checkFiles()
}

func TestJournalCryptoConcurrentReservationsCannotExceedCap(t *testing.T) {
	s, _, _, _ := newCryptoJournal(t)
	if _, err := s.db.Exec(`UPDATE event_keys SET uses=?`, maxKeyUses-1); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.sealEvent(context.Background(), cryptoBinding(), []byte("body"))
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrKeyExhausted) {
				t.Errorf("unexpected reservation error: %v", err)
			}
		}()
	}
	wg.Wait()
	var uses int64
	if err := s.db.QueryRow(`SELECT uses FROM event_keys`).Scan(&uses); err != nil || uses != maxKeyUses || successes.Load() != 1 {
		t.Fatalf("reservation cap violated: successes=%d uses=%d err=%v", successes.Load(), uses, err)
	}
}
