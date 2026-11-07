package operations

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func storageBinding() blobBinding {
	return blobBinding{OperationID: "00000000-0000-4000-8000-000000000004", NodeID: "00000000-0000-4000-8000-000000000002", Digest: "opaque-digest", Kind: "result", Seq: 2}
}

func TestCryptoKeepsOriginalBytesAndIndependentEnvelopeKeys(t *testing.T) {
	s, _, keys := storageFixture(t)
	b, raw := storageBinding(), []byte(" {\n\"secret\":\"manager-private-fab312\",\"seq\":2.0 }")
	want := hmac.New(sha256.New, keys.DigestKey)
	want.Write(raw)
	digest, err := s.InputDigest(raw)
	if err != nil || digest != base64.RawURLEncoding.EncodeToString(want.Sum(nil)) {
		t.Fatalf("wrong original digest: %v", err)
	}
	first, err := s.seal(context.Background(), b, raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.seal(context.Background(), b, raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Cipher, second.Cipher) || bytes.Equal(first.WrappedDEK, second.WrappedDEK) {
		t.Fatal("encryption reused randomness")
	}
	aad := func(domain string) []byte {
		data, _ := json.Marshal([]any{domain, s.identity.ManagerID, b.OperationID, b.NodeID, b.Digest, b.Kind, b.Seq, keys.EncryptionID})
		return data
	}
	block, _ := aes.NewCipher(keys.EncryptionKey)
	wrap, _ := cipher.NewGCMWithRandomNonce(block)
	dek, err := wrap.Open(nil, nil, first.WrappedDEK, aad("dm-manager-dek-v1"))
	if err != nil || len(dek) != 32 {
		t.Fatalf("wrong wrapped DEK: %v", err)
	}
	defer clear(dek)
	block, _ = aes.NewCipher(dek)
	data, _ := cipher.NewGCMWithRandomNonce(block)
	got, err := data.Open(nil, nil, first.Cipher, aad("dm-manager-body-v1"))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("wrong body/AAD: %v", err)
	}
	keys.DigestKey[0] ^= 1
	keys.EncryptionKey[0] ^= 1
	got, err = s.unseal(b, first)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("caller mutated owned keys: %v", err)
	}
}

func TestCryptoRejectsTamperingWithoutReturningPlaintext(t *testing.T) {
	s, _, _ := storageFixture(t)
	b := storageBinding()
	original, err := s.seal(context.Background(), b, []byte("sensitive body"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"cipher", "dek", "fingerprint", "key", "operation", "node", "digest", "kind", "seq", "purpose"} {
		t.Run(field, func(t *testing.T) {
			e := sealedBlob{Cipher: bytes.Clone(original.Cipher), WrappedDEK: bytes.Clone(original.WrappedDEK), Fingerprint: bytes.Clone(original.Fingerprint), KeyID: original.KeyID}
			binding := b
			switch field {
			case "cipher":
				e.Cipher[0] ^= 1
			case "dek":
				e.WrappedDEK[0] ^= 1
			case "fingerprint":
				e.Fingerprint[0] ^= 1
			case "key":
				e.KeyID += "bad"
			case "operation":
				binding.OperationID += "bad"
			case "node":
				binding.NodeID += "bad"
			case "digest":
				binding.Digest += "bad"
			case "kind":
				binding.Kind = "payload"
			case "seq":
				binding.Seq++
			case "purpose":
				e.Cipher, e.WrappedDEK = e.WrappedDEK, e.Cipher
			}
			got, err := s.unseal(binding, e)
			if !errors.Is(err, ErrEvidence) || len(got) != 0 {
				t.Fatalf("tampering returned evidence: %q %v", got, err)
			}
		})
	}
}

func TestCryptoReservationSurvivesRollbackReopenAndExhaustion(t *testing.T) {
	s, path, keys := storageFixture(t)
	ctx, b := context.Background(), storageBinding()
	e, err := s.seal(ctx, b, []byte("reservation-body"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE keys SET uses=uses+1`); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	var uses int64
	if err := s.db.QueryRow(`SELECT uses FROM keys`).Scan(&uses); err != nil || uses != 1 {
		t.Fatalf("refunded quota: %d %v", uses, err)
	}
	if _, err := s.db.Exec(`UPDATE keys SET uses=?`, maxKeyUses); err != nil {
		t.Fatal(err)
	}
	if _, err := s.seal(ctx, b, []byte("new body")); !errors.Is(err, ErrKeyExhausted) {
		t.Fatalf("over cap: %v", err)
	}
	id := s.Identity()
	s.Close()
	opened, err := Open(ctx, path, id, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	got, err := opened.unseal(b, e)
	if err != nil || string(got) != "reservation-body" {
		t.Fatalf("exhaustion blocked read: %v", err)
	}
	if err := opened.db.QueryRow(`SELECT uses FROM keys`).Scan(&uses); err != nil || uses != maxKeyUses {
		t.Fatalf("reopen lost quota: %d %v", uses, err)
	}
}

func TestCryptoReservationFailureNeverReturnsCiphertext(t *testing.T) {
	for _, commitFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "statement", true: "commit"}[commitFailure], func(t *testing.T) {
			s, _, _ := storageFixture(t)
			stmt := `CREATE TRIGGER fail_key BEFORE UPDATE ON keys BEGIN SELECT RAISE(ABORT,'private-key-secret'); END`
			if commitFailure {
				stmt = `CREATE TABLE key_fault(parent TEXT REFERENCES users(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_key AFTER UPDATE ON keys BEGIN INSERT INTO key_fault(parent) VALUES('private-key-secret'); END`
			}
			if _, err := s.db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
			e, err := s.seal(context.Background(), storageBinding(), []byte("secret"))
			if !errors.Is(err, ErrStorage) || len(e.Cipher) > 0 || len(e.WrappedDEK) > 0 || strings.Contains(err.Error(), "private-key-secret") {
				t.Fatalf("reservation failure leaked result: %v", err)
			}
			var uses int64
			if err := s.db.QueryRow(`SELECT uses FROM keys`).Scan(&uses); err != nil || uses != 0 {
				t.Fatalf("failed reservation changed quota: %d %v", uses, err)
			}
		})
	}
}

func TestCryptoConcurrentReservationCannotExceedBudget(t *testing.T) {
	s, _, _ := storageFixture(t)
	if _, err := s.db.Exec(`UPDATE keys SET uses=?`, maxKeyUses-1); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.seal(context.Background(), storageBinding(), []byte("body"))
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrKeyExhausted) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var uses int64
	if err := s.db.QueryRow(`SELECT uses FROM keys`).Scan(&uses); err != nil || uses != maxKeyUses || successes.Load() != 1 {
		t.Fatalf("cap violated: successes %d uses %d err %v", successes.Load(), uses, err)
	}
}

func TestCryptoKeepsSecretsOutOfDatabaseAndWAL(t *testing.T) {
	s, path, keys := storageFixture(t)
	secret := []byte("manager-private-body-54c9dc08dd5c3d")
	e, err := s.seal(context.Background(), storageBinding(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE crypto_probe(body BLOB,wrapped BLOB,fingerprint BLOB); INSERT INTO crypto_probe VALUES(?,?,?)`, e.Cipher, e.WrappedDEK, e.Fingerprint); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		for _, p := range []string{path, path + "-wal"} {
			raw, err := os.ReadFile(p)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range [][]byte{secret, keys.DigestKey, keys.EncryptionKey} {
				if bytes.Contains(raw, private) {
					t.Fatal("database contains plaintext secret")
				}
			}
		}
	}
	check()
	s.Close()
	check()
}
