package operations

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
)

const maxKeyUses int64 = 1048576
const maxBlobBytes = 8 << 20

type blobBinding struct {
	OperationID, NodeID, Digest, Kind string
	Seq                               uint64
}
type sealedBlob struct {
	Cipher, WrappedDEK, Fingerprint []byte
	KeyID                           string
}

func (s *Store) keyCopies() (Keys, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return Keys{}, ErrStorage
	}
	return Keys{DigestID: s.keys.DigestID, EncryptionID: s.keys.EncryptionID, DigestKey: append([]byte(nil), s.keys.DigestKey...), EncryptionKey: append([]byte(nil), s.keys.EncryptionKey...)}, nil
}

func rawFingerprint(key, raw []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("dm-manager-fingerprint-v1\x00"))
	mac.Write(raw)
	return mac.Sum(nil)
}
func (s *Store) fingerprint(raw []byte) ([]byte, error) {
	keys, err := s.keyCopies()
	if err != nil {
		return nil, err
	}
	defer clear(keys.DigestKey)
	defer clear(keys.EncryptionKey)
	return rawFingerprint(keys.DigestKey, raw), nil
}

// InputDigest authenticates precisely the frozen payload bytes. Unlike local
// fingerprints, its input has no purpose prefix and is never re-encoded.
func (s *Store) InputDigest(raw []byte) (string, error) {
	keys, err := s.keyCopies()
	if err != nil {
		return "", err
	}
	defer clear(keys.DigestKey)
	defer clear(keys.EncryptionKey)
	mac := hmac.New(sha256.New, keys.DigestKey)
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Store) blobAAD(domain string, b blobBinding, keyID string) []byte {
	raw, _ := json.Marshal([]any{domain, s.identity.ManagerID, b.OperationID, b.NodeID, b.Digest, b.Kind, b.Seq, keyID})
	return raw
}
func blobAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrEvidence
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, ErrEvidence
	}
	return aead, nil
}

// reserveKeyUse commits before any wrapping. It must never be called while a
// transaction holds this Store's sole connection. Failure and rollback of the
// subsequent business transaction deliberately do not refund this reservation.
func (s *Store) reserveKeyUse(ctx context.Context, keyID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err := s.requireActive(ctx, tx); err != nil {
		return err
	}
	var actual string
	var uses int64
	if err := tx.QueryRowContext(ctx, `SELECT encryption_key_id,uses FROM keys WHERE singleton=1`).Scan(&actual, &uses); err != nil {
		return ErrStorage
	}
	if actual != keyID || uses < 0 || uses > maxKeyUses {
		return ErrEvidence
	}
	if uses == maxKeyUses {
		return ErrKeyExhausted
	}
	result, err := tx.ExecContext(ctx, `UPDATE keys SET uses=uses+1 WHERE singleton=1 AND encryption_key_id=? AND uses=?`, keyID, uses)
	if err != nil {
		return ErrStorage
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return ErrStorage
	}
	if err := tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

func (s *Store) seal(ctx context.Context, b blobBinding, raw []byte) (sealedBlob, error) {
	if len(raw) > maxBlobBytes {
		return sealedBlob{}, ErrInput
	}
	keys, err := s.keyCopies()
	if err != nil {
		return sealedBlob{}, err
	}
	defer clear(keys.DigestKey)
	defer clear(keys.EncryptionKey)
	if err := s.reserveKeyUse(ctx, keys.EncryptionID); err != nil {
		return sealedBlob{}, err
	}
	dek := make([]byte, 32)
	defer clear(dek)
	if _, err := rand.Read(dek); err != nil {
		return sealedBlob{}, ErrStorage
	}
	data, err := blobAEAD(dek)
	if err != nil {
		return sealedBlob{}, ErrStorage
	}
	wrap, err := blobAEAD(keys.EncryptionKey)
	if err != nil {
		return sealedBlob{}, ErrStorage
	}
	return sealedBlob{Cipher: data.Seal(nil, nil, raw, s.blobAAD("dm-manager-body-v1", b, keys.EncryptionID)), WrappedDEK: wrap.Seal(nil, nil, dek, s.blobAAD("dm-manager-dek-v1", b, keys.EncryptionID)), Fingerprint: rawFingerprint(keys.DigestKey, raw), KeyID: keys.EncryptionID}, nil
}

// unseal does no database work, so readers may use it within a snapshot
// transaction. No plaintext escapes unless both AEAD layers and fingerprint fit.
func (s *Store) unseal(b blobBinding, e sealedBlob) ([]byte, error) {
	if len(e.Cipher) < 28 || len(e.Cipher) > maxBlobBytes+28 || len(e.WrappedDEK) != 60 || len(e.Fingerprint) != sha256.Size {
		return nil, ErrEvidence
	}
	keys, err := s.keyCopies()
	if err != nil {
		return nil, err
	}
	defer clear(keys.DigestKey)
	defer clear(keys.EncryptionKey)
	if e.KeyID != keys.EncryptionID {
		return nil, ErrEvidence
	}
	wrap, err := blobAEAD(keys.EncryptionKey)
	if err != nil {
		return nil, ErrEvidence
	}
	dek, err := wrap.Open(nil, nil, e.WrappedDEK, s.blobAAD("dm-manager-dek-v1", b, e.KeyID))
	if err != nil {
		return nil, ErrEvidence
	}
	defer clear(dek)
	if len(dek) != 32 {
		return nil, ErrEvidence
	}
	data, err := blobAEAD(dek)
	if err != nil {
		return nil, ErrEvidence
	}
	raw, err := data.Open(nil, nil, e.Cipher, s.blobAAD("dm-manager-body-v1", b, e.KeyID))
	if err != nil {
		return nil, ErrEvidence
	}
	if !hmac.Equal(rawFingerprint(keys.DigestKey, raw), e.Fingerprint) {
		clear(raw)
		return nil, ErrEvidence
	}
	return raw, nil
}
