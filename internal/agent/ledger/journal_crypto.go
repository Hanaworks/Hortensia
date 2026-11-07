package ledger

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
)

type eventBinding struct {
	OperationID, Digest, Kind string
	Seq                       uint64
}

type sealedEvent struct {
	Cipher, WrappedDEK, Fingerprint []byte
	KeyID                           string
}

func (s *Store) eventKeyCopies() (keyID string, kek, fingerprintKey []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return "", nil, nil, ErrStorage
	}
	if s.journal == nil {
		return "", nil, nil, ErrJournal
	}
	return s.journal.keyID, append([]byte(nil), s.journal.kek...), append([]byte(nil), s.key...), nil
}

func (s *Store) eventAAD(domain string, b eventBinding, keyID string) []byte {
	raw, _ := json.Marshal([]any{domain, s.identity.NodeID, s.identity.LedgerID, b.OperationID, b.Digest, b.Kind, b.Seq, keyID})
	return raw
}

func eventAEAD(key []byte) (cipher.AEAD, error) {
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

// reserveKeyUse commits independently before any encryption. This reservation
// is deliberately not refunded when encryption or the later event transaction
// fails. Callers must not hold a transaction on this Store's sole connection.
func (s *Store) reserveKeyUse(ctx context.Context, keyID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err := s.requireRecoveryWriter(ctx, tx); err != nil {
		return err
	}
	var actualID string
	var uses int64
	if err := tx.QueryRowContext(ctx, `SELECT key_id,uses FROM event_keys WHERE id=1`).Scan(&actualID, &uses); err != nil {
		return ErrStorage
	}
	if actualID != keyID || uses < 0 || uses > maxKeyUses {
		return ErrEvidence
	}
	if uses == maxKeyUses {
		return ErrKeyExhausted
	}
	result, err := tx.ExecContext(ctx, `UPDATE event_keys SET uses=uses+1 WHERE id=1 AND key_id=? AND uses=?`, keyID, uses)
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

func (s *Store) sealEvent(ctx context.Context, b eventBinding, body []byte) (sealedEvent, error) {
	keyID, kek, ledgerKey, err := s.eventKeyCopies()
	if err != nil {
		return sealedEvent{}, err
	}
	defer clear(kek)
	defer clear(ledgerKey)
	if err := s.reserveKeyUse(ctx, keyID); err != nil {
		return sealedEvent{}, err
	}
	dek := make([]byte, 32)
	defer clear(dek)
	if _, err := rand.Read(dek); err != nil {
		return sealedEvent{}, ErrStorage
	}
	data, err := eventAEAD(dek)
	if err != nil {
		return sealedEvent{}, ErrStorage
	}
	wrap, err := eventAEAD(kek)
	if err != nil {
		return sealedEvent{}, ErrStorage
	}
	mac := hmac.New(sha256.New, ledgerKey)
	mac.Write(body)
	return sealedEvent{
		Cipher:      data.Seal(nil, nil, body, s.eventAAD("dm-event-body-v1", b, keyID)),
		WrappedDEK:  wrap.Seal(nil, nil, dek, s.eventAAD("dm-event-dek-v1", b, keyID)),
		Fingerprint: mac.Sum(nil), KeyID: keyID,
	}, nil
}

// openEvent performs no database access and is safe within a transaction. It
// returns no plaintext unless both AEAD layers and the body fingerprint agree.
func (s *Store) openEvent(b eventBinding, e sealedEvent) ([]byte, error) {
	keyID, kek, ledgerKey, err := s.eventKeyCopies()
	if err != nil {
		return nil, err
	}
	defer clear(kek)
	defer clear(ledgerKey)
	if e.KeyID != keyID || len(e.Fingerprint) != sha256.Size {
		return nil, ErrEvidence
	}
	wrap, err := eventAEAD(kek)
	if err != nil {
		return nil, ErrEvidence
	}
	dek, err := wrap.Open(nil, nil, e.WrappedDEK, s.eventAAD("dm-event-dek-v1", b, keyID))
	if err != nil {
		return nil, ErrEvidence
	}
	defer clear(dek)
	if len(dek) != 32 {
		return nil, ErrEvidence
	}
	data, err := eventAEAD(dek)
	if err != nil {
		return nil, ErrEvidence
	}
	body, err := data.Open(nil, nil, e.Cipher, s.eventAAD("dm-event-body-v1", b, keyID))
	if err != nil {
		return nil, ErrEvidence
	}
	mac := hmac.New(sha256.New, ledgerKey)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), e.Fingerprint) {
		clear(body)
		return nil, ErrEvidence
	}
	return body, nil
}
