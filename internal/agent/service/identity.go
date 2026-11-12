package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/localstate"
	"docker-manager/internal/protocol"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

type Identity struct {
	FormatVersion        int    `json:"format_version"`
	SchemaVersion        int    `json:"schema_version"`
	NodeID               string `json:"node_id"`
	LedgerID             string `json:"ledger_id"`
	LedgerKeyID          string `json:"ledger_key_id"`
	JournalKeyID         string `json:"journal_key_id"`
	ManagerOrigin        string `json:"manager_origin"`
	CredentialState      string `json:"credential_state"`
	ActiveSecret         string `json:"active_secret,omitempty"`
	CandidateSecret      string `json:"candidate_secret,omitempty"`
	CredentialGeneration uint64 `json:"credential_generation"`
}
type keys struct {
	ledger  []byte
	journal ledger.JournalKey
}

func clearKeys(k *keys) { clear(k.ledger); clear(k.journal.Key) }
func newSecret() (string, error) {
	b := make([]byte, 32)
	defer clear(b)
	if _, e := rand.Read(b); e != nil {
		return "", ErrIdentity
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func validSecret(s string) bool {
	b, e := base64.RawURLEncoding.Strict().DecodeString(s)
	defer clear(b)
	return e == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == s
}

// Match the shared Schema Id: lowercase UUIDv4 with the RFC 4122 variant.
var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func Initialize(ctx context.Context, c Config, nodeID string) (Identity, error) {
	if validateConfig(c) != nil || !idPattern.MatchString(nodeID) {
		return Identity{}, ErrConfig
	}
	for _, d := range []string{c.StateDir, c.KeysDir, filepath.Dir(c.IdentityFile)} {
		if e := localstate.EnsurePrivateDir(d); e != nil {
			return Identity{}, e
		}
	}
	lock, e := localstate.AcquireLock(filepath.Join(c.StateDir, ".agent.lock"))
	if e != nil {
		return Identity{}, e
	}
	defer lock.Close()
	for _, p := range []string{c.IdentityFile, filepath.Join(c.StateDir, "agent.db"), filepath.Join(c.StateDir, "agent.db-wal"), filepath.Join(c.StateDir, "agent.db-shm"), filepath.Join(c.StateDir, "agent.db-journal"), filepath.Join(c.KeysDir, "ledger.key"), filepath.Join(c.KeysDir, "journal.key")} {
		if localstate.RejectLinks(p) != nil {
			return Identity{}, ErrIdentity
		}
		if _, e := os.Lstat(p); !os.IsNotExist(e) {
			return Identity{}, localstate.ErrInitialized
		}
	}
	kid, e := protocol.NewID()
	if e != nil {
		return Identity{}, e
	}
	jid, e := protocol.NewID()
	if e != nil {
		return Identity{}, e
	}
	k := keys{make([]byte, 32), ledger.JournalKey{ID: jid, Key: make([]byte, 32)}}
	defer clearKeys(&k)
	if _, e = rand.Read(k.ledger); e != nil {
		return Identity{}, ErrIdentity
	}
	if _, e = rand.Read(k.journal.Key); e != nil {
		return Identity{}, ErrIdentity
	}
	if e = localstate.WriteExclusive(filepath.Join(c.KeysDir, "ledger.key"), k.ledger); e != nil {
		return Identity{}, e
	}
	if e = localstate.WriteExclusive(filepath.Join(c.KeysDir, "journal.key"), k.journal.Key); e != nil {
		return Identity{}, e
	}
	s, e := ledger.CreateRuntime(ctx, filepath.Join(c.StateDir, "agent.db"), nodeID, kid, k.ledger, k.journal, ledger.RuntimeOptions{})
	if e != nil {
		return Identity{}, e
	}
	lid := s.Identity().LedgerID
	if e = s.Close(); e != nil {
		return Identity{}, e
	}
	if e = localstate.SyncDirectory(c.StateDir); e != nil {
		return Identity{}, e
	}
	secret, e := newSecret()
	if e != nil {
		return Identity{}, e
	}
	id := Identity{FormatVersion: 1, SchemaVersion: 5, NodeID: nodeID, LedgerID: lid, LedgerKeyID: kid, JournalKeyID: jid, ManagerOrigin: c.ManagerOrigin, CredentialState: "candidate", CandidateSecret: secret}
	b, _ := json.Marshal(id)
	if e = localstate.WriteExclusive(c.IdentityFile, b); e != nil {
		return Identity{}, e
	}
	return id, nil
}
func loadIdentity(c Config) (Identity, keys, error) {
	var id Identity
	k := keys{}
	fail := func() (Identity, keys, error) { clearKeys(&k); return Identity{}, keys{}, ErrIdentity }
	b, e := localstate.ReadPrivateFile(c.IdentityFile, 8192)
	if e != nil {
		return fail()
	}
	value, e := protocol.StrictJSON(b)
	if e != nil {
		return fail()
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fail()
	}
	for name, value := range object {
		if value == nil {
			return fail()
		}
		switch name {
		case "format_version", "schema_version", "node_id", "ledger_id", "ledger_key_id", "journal_key_id", "manager_origin", "credential_state", "active_secret", "candidate_secret", "credential_generation":
		default:
			return fail()
		}
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&id) != nil || id.FormatVersion != 1 || id.SchemaVersion != 5 || !idPattern.MatchString(id.NodeID) || !idPattern.MatchString(id.LedgerID) || id.LedgerKeyID == "" || id.JournalKeyID == "" || id.LedgerKeyID == id.JournalKeyID || id.ManagerOrigin != c.ManagerOrigin {
		return fail()
	}
	switch id.CredentialState {
	case "candidate":
		if id.ActiveSecret != "" || !validSecret(id.CandidateSecret) || id.CredentialGeneration != 0 {
			return fail()
		}
	case "active":
		if !validSecret(id.ActiveSecret) || id.CandidateSecret != "" || id.CredentialGeneration < 1 {
			return fail()
		}
	case "rotation_pending":
		if !validSecret(id.ActiveSecret) || !validSecret(id.CandidateSecret) || id.ActiveSecret == id.CandidateSecret || id.CredentialGeneration < 1 {
			return fail()
		}
	default:
		return fail()
	}
	k.ledger, e = localstate.ReadPrivateFile(filepath.Join(c.KeysDir, "ledger.key"), 32)
	if e != nil || len(k.ledger) != 32 {
		return fail()
	}
	k.journal.ID = id.JournalKeyID
	k.journal.Key, e = localstate.ReadPrivateFile(filepath.Join(c.KeysDir, "journal.key"), 32)
	if e != nil || len(k.journal.Key) != 32 {
		return fail()
	}
	return id, k, nil
}
func (id Identity) ledgerIdentity() ledger.Identity {
	return ledger.Identity{NodeID: id.NodeID, LedgerID: id.LedgerID, KeyID: id.LedgerKeyID}
}
func saveIdentity(c Config, id Identity) error {
	b, e := json.Marshal(id)
	if e != nil {
		return ErrIdentity
	}
	return localstate.Replace(c.IdentityFile, b)
}
