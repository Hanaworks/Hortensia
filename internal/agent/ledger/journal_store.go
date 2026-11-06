package ledger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"unicode/utf8"
)

//go:embed migrations/002_event_journal.sql
var schemaV2 string

// JournalKey is an independently generated, per-ledger 32-byte KEK. Its
// lifecycle and backup are the caller's responsibility; it must differ from
// K_ledger. Stores copy the key and never persist it in the database.
type JournalKey struct {
	ID  string
	Key []byte
}

// The pointer and key ID remain immutable for a Store's lifetime. Access to
// kek is protected by Store.mu because Close clears that owned byte slice.
type journalConfig struct {
	keyID string
	kek   []byte
}

const maxKeyUses int64 = 1048576

func validateJournalKey(journalKey JournalKey, ledgerKey []byte) error {
	if journalKey.ID == "" || len(journalKey.ID) > 128 || !utf8.ValidString(journalKey.ID) || len(journalKey.Key) != 32 || hmac.Equal(journalKey.Key, ledgerKey) {
		return ErrKey
	}
	return nil
}

func journalKeyCheck(identity Identity, key JournalKey) []byte {
	raw, _ := json.Marshal([]string{identity.NodeID, identity.LedgerID, key.ID})
	mac := hmac.New(sha256.New, key.Key)
	mac.Write([]byte("dm-event-key-v1\x00"))
	mac.Write(raw)
	return mac.Sum(nil)
}

// CreateJournal creates a new version-2 controlled ledger. It is not an
// upgrade or replacement path for a node with an existing version-1 ledger.
func CreateJournal(ctx context.Context, path, nodeID, keyID string, key []byte, journalKey JournalKey) (*Store, error) {
	return createStore(ctx, path, nodeID, keyID, key, &journalKey)
}

// OpenJournal validates both identities and actual keys, including an empty
// journal. It refuses version 1 and performs no repair or migration.
func OpenJournal(ctx context.Context, path string, expected Identity, key []byte, journalKey JournalKey) (*Store, error) {
	return openStore(ctx, path, expected, key, &journalKey)
}

func (s *Store) requireJournal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return ErrStorage
	}
	if s.journal == nil {
		return ErrJournal
	}
	return nil
}
