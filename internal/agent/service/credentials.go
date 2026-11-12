package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/agentwire"
	"docker-manager/internal/localstate"
	"docker-manager/internal/protocol"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func rootCAs(c Config) (*x509.CertPool, error) {
	if c.CAFile == "" {
		return nil, nil
	}
	if localstate.RejectLinks(c.CAFile) != nil {
		return nil, ErrConfig
	}
	f, e := os.Open(c.CAFile)
	if e != nil {
		return nil, ErrConfig
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Size() > 1<<20 {
		return nil, ErrConfig
	}
	b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return nil, ErrConfig
	}
	pool, e := x509.SystemCertPool()
	if e != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(b) {
		return nil, ErrConfig
	}
	return pool, nil
}
func wireMessage(kind, session, reply, id string, body any) ([]byte, error) {
	if id == "" {
		var e error
		id, e = protocol.NewID()
		if e != nil {
			return nil, e
		}
	}
	var ss, rr any
	if session != "" {
		ss = session
	}
	if reply != "" {
		rr = reply
	}
	return json.Marshal(map[string]any{"protocol": 1, "type": kind, "message_id": id, "session_id": ss, "reply_to": rr, "body": body})
}
func hello(s *ledger.Store, p ledger.RuntimeProfile, caps []string) ([]byte, string, error) {
	mid, e := protocol.NewID()
	if e != nil {
		return nil, "", e
	}
	if caps == nil {
		caps = []string{}
	}
	cv := p.ComposeVersion
	if cv == "" {
		cv = "unavailable"
	}
	id := s.Identity()
	b, e := wireMessage("hello", "", "", mid, map[string]any{"node_id": id.NodeID, "ledger_id": id.LedgerID, "boot_id": s.BootID(), "agent_version": "m1-dev", "compose_version": cv, "capabilities": caps, "execution_profile": map[string]any{"docker_endpoint_id": p.EndpointID, "policy_revision": p.PolicyRevision}})
	return b, mid, e
}
func authenticate(ctx context.Context, c Config, id Identity, s *ledger.Store, secret string) error {
	p, e := s.RuntimeProfile(ctx)
	if e != nil {
		return e
	}
	b, _, e := hello(s, p, nil)
	if e != nil {
		return e
	}
	ca, e := rootCAs(c)
	if e != nil {
		return e
	}
	peer, _, e := agentwire.Dial(ctx, agentwire.ClientConfig{URL: "wss" + strings.TrimPrefix(c.ManagerOrigin, "https") + "/agent/v1/connect", Token: secret, RootCAs: ca, Hello: b})
	if e != nil {
		if errors.Is(e, agentwire.ErrUnauthorized) {
			return ErrUnauthorized
		}
		return ErrTransport
	}
	peer.Close()
	return nil
}
func openIdentity(ctx context.Context, c Config) (Identity, keys, *ledger.Store, *localstate.Lock, error) {
	if validateConfig(c) != nil {
		return Identity{}, keys{}, nil, nil, ErrConfig
	}
	if localstate.CheckPrivateDir(c.StateDir) != nil {
		return Identity{}, keys{}, nil, nil, ErrIdentity
	}
	lock, e := localstate.AcquireLock(filepath.Join(c.StateDir, ".agent.lock"))
	if e != nil {
		return Identity{}, keys{}, nil, nil, e
	}
	id, k, e := loadIdentity(c)
	if e != nil {
		lock.Close()
		return id, k, nil, nil, e
	}
	if e = localstate.CheckPrivateFile(filepath.Join(c.StateDir, "agent.db")); e != nil {
		clearKeys(&k)
		lock.Close()
		return Identity{}, keys{}, nil, nil, ErrIdentity
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		p := filepath.Join(c.StateDir, "agent.db") + suffix
		if localstate.RejectLinks(p) != nil {
			clearKeys(&k)
			lock.Close()
			return Identity{}, keys{}, nil, nil, ErrIdentity
		}
		if _, err := os.Lstat(p); err != nil && !os.IsNotExist(err) || err == nil && localstate.CheckPrivateFile(p) != nil {
			clearKeys(&k)
			lock.Close()
			return Identity{}, keys{}, nil, nil, ErrIdentity
		}
	}
	s, e := ledger.OpenRuntime(ctx, filepath.Join(c.StateDir, "agent.db"), id.ledgerIdentity(), k.ledger, k.journal, ledger.RuntimeOptions{})
	if e != nil {
		clearKeys(&k)
		lock.Close()
		return Identity{}, keys{}, nil, nil, e
	}
	return id, k, s, lock, nil
}
func credentialRequest(ctx context.Context, c Config, path, token string, body any, id Identity, expected uint64) error {
	ca, e := rootCAs(c)
	if e != nil {
		return e
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca, MinVersion: tls.VersionTLS12}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	b, e := json.Marshal(body)
	if e != nil {
		return ErrIdentity
	}
	req, e := http.NewRequestWithContext(ctx, "POST", c.ManagerOrigin+path, bytes.NewReader(b))
	if e != nil {
		return ErrConfig
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	r, e := client.Do(req)
	if e != nil {
		return ErrTransport
	}
	defer r.Body.Close()
	if r.StatusCode == 401 {
		return ErrUnauthorized
	}
	if r.StatusCode != 200 {
		return ErrTransport
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, 8193))
	if e != nil || len(raw) > 8192 {
		return ErrProtocol
	}
	if _, e = protocol.StrictJSON(raw); e != nil {
		return ErrProtocol
	}
	var response struct {
		NodeID     string `json:"node_id"`
		LedgerID   string `json:"ledger_id"`
		Generation uint64 `json:"credential_generation"`
	}
	if json.Unmarshal(raw, &response) != nil || response.NodeID != id.NodeID || response.LedgerID != id.LedgerID || response.Generation != expected {
		return ErrProtocol
	}
	return nil
}
func promote(c Config, id Identity, generation uint64) error {
	id.ActiveSecret = id.CandidateSecret
	id.CandidateSecret = ""
	id.CredentialGeneration = generation
	id.CredentialState = "active"
	return saveIdentity(c, id)
}
func Enroll(ctx context.Context, c Config, token string) error {
	id, k, s, lock, e := openIdentity(ctx, c)
	if e != nil {
		return e
	}
	defer lock.Close()
	defer clearKeys(&k)
	defer s.Close()
	if id.CredentialState != "candidate" {
		return ErrIdentity
	}
	e = authenticate(ctx, c, id, s, id.CandidateSecret)
	if e == nil {
		return promote(c, id, 1)
	}
	if !errors.Is(e, ErrUnauthorized) {
		return e
	}
	if !validSecret(token) {
		return ErrConfig
	}
	e = credentialRequest(ctx, c, "/agent/v1/enroll", token, map[string]any{"node_id": id.NodeID, "ledger_id": id.LedgerID, "credential_secret": id.CandidateSecret}, id, 1)
	if e != nil {
		return e
	}
	return promote(c, id, 1)
}
func RotateCredentials(ctx context.Context, c Config) error {
	id, k, s, lock, e := openIdentity(ctx, c)
	if e != nil {
		return e
	}
	defer lock.Close()
	defer clearKeys(&k)
	defer s.Close()
	if id.CredentialState == "candidate" {
		return ErrIdentity
	}
	if id.CredentialState == "active" {
		candidate, e := newSecret()
		if e != nil {
			return e
		}
		id.CandidateSecret = candidate
		id.CredentialState = "rotation_pending"
		if e = saveIdentity(c, id); e != nil {
			return e
		}
	}
	e = authenticate(ctx, c, id, s, id.CandidateSecret)
	if e == nil {
		return promote(c, id, id.CredentialGeneration+1)
	}
	if !errors.Is(e, ErrUnauthorized) {
		return e
	}
	if e = authenticate(ctx, c, id, s, id.ActiveSecret); e != nil {
		return e
	}
	e = credentialRequest(ctx, c, "/agent/v1/credentials/rotate", id.ActiveSecret, map[string]any{"ledger_id": id.LedgerID, "credential_secret": id.CandidateSecret}, id, id.CredentialGeneration+1)
	if e != nil {
		return e
	}
	return promote(c, id, id.CredentialGeneration+1)
}
