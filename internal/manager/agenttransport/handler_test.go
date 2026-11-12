package agenttransport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/agentwire"
	"docker-manager/internal/manager/operations"
	"docker-manager/internal/protocol"
)

func id(t *testing.T) string {
	t.Helper()
	v, err := protocol.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func reply(t *testing.T, r operations.ServiceReply) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func mutate(t *testing.T, s *operations.Store, b *operations.BrowserSession, path string, body any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	r, err := s.DoService(context.Background(), b, operations.ServiceRequest{Method: "POST", Path: path, Body: raw, IdempotencyKey: id(t)})
	if err != nil {
		t.Fatal(err)
	}
	return reply(t, r)
}

func TestRealCredentialAndAgentStoreCompleteTLSRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	m, err := operations.CreateRuntimeService(ctx, filepath.Join(dir, "manager.db"), id(t), operations.Keys{DigestID: "digest", EncryptionID: "encryption", DigestKey: bytes.Repeat([]byte{1}, 32), EncryptionKey: bytes.Repeat([]byte{2}, 32)}, operations.Bootstrap{Username: "admin", Password: "example password"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	login, err := m.Login(ctx, "admin", "example password")
	if err != nil {
		t.Fatal(err)
	}
	node := mutate(t, m, login.Session, "/api/v1/nodes", map[string]any{"display_name": "test node"})["node_id"].(string)
	a, err := ledger.CreateRuntime(ctx, filepath.Join(dir, "agent.db"), node, "ledger", bytes.Repeat([]byte{3}, 32), ledger.JournalKey{ID: "journal", Key: bytes.Repeat([]byte{4}, 32)}, ledger.RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	token := mutate(t, m, login.Session, "/api/v1/nodes/"+node+"/enrollment-tokens", map[string]any{})["token"].(string)
	secret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	if _, err := m.EnrollNode(ctx, token, operations.EnrollmentRequest{NodeID: node, LedgerID: a.Identity().LedgerID, CredentialSecret: secret}); err != nil {
		t.Fatal(err)
	}
	h, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(h)
	defer server.Close()
	defer h.Close(context.Background())
	profile, err := a.RuntimeProfile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := protocol.NewMessage("hello", "", "", map[string]any{"node_id": node, "ledger_id": a.Identity().LedgerID, "boot_id": a.BootID(), "agent_version": "controlled-test", "compose_version": "unavailable", "capabilities": []string{}, "execution_profile": map[string]any{"docker_endpoint_id": profile.EndpointID, "policy_revision": profile.PolicyRevision}})
	p, welcome, err := agentwire.Dial(ctx, agentwire.ClientConfig{URL: "wss" + strings.TrimPrefix(server.URL, "https") + "/agent/v1/connect", Token: secret, Hello: hello, RootCAs: server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var he protocol.Envelope
	json.Unmarshal(hello, &he)
	if err := a.BindManagerSession(ctx, welcome, he.MessageID); err != nil {
		t.Fatal(err)
	}
	complete := false
	for !complete {
		raw, e, err := p.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var answer []byte
		switch e.Type {
		case "reconcile":
			answer, err = a.Reconcile(ctx, raw)
		case "reconcile_complete":
			answer, err = a.CompleteReconcile(ctx, raw)
			complete = true
		default:
			t.Fatal("unexpected startup message", e.Type)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Write(ctx, answer); err != nil {
			t.Fatal(err)
		}
	}
	b := operations.Binding{NodeID: node, LedgerID: a.Identity().LedgerID, SessionID: p.SessionID()}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := m.ReadRuntimeConnectionState(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if !state.Frozen && state.Recovery.Status == "completed" {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("Manager did not persist completed handshake")
		}
	}
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTransportRejectsInvalidHTTPBeforeStore(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil Store accepted")
	}
}
