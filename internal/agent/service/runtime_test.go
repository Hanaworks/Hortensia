package service

import (
	"context"
	"crypto/x509"
	"docker-manager/internal/agentwire"
	"docker-manager/internal/protocol"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServeUsesDurableIdentityAndActualRecovery(t *testing.T) {
	c := testConfig(t)
	done := make(chan error, 1)
	var expected Identity
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, e := agentwire.Accept(w, r)
		if e != nil {
			done <- e
			return
		}
		defer h.Close()
		raw, e := h.ReadHello(r.Context())
		if e != nil {
			done <- e
			return
		}
		v, _ := protocol.NewValidator()
		env, e := v.Validate(raw)
		if e != nil {
			done <- e
			return
		}
		var b struct {
			Node, Ledger, Boot string
			Caps               []string
		}
		var obj map[string]json.RawMessage
		json.Unmarshal(env.Body, &obj)
		json.Unmarshal(obj["node_id"], &b.Node)
		json.Unmarshal(obj["ledger_id"], &b.Ledger)
		json.Unmarshal(obj["boot_id"], &b.Boot)
		json.Unmarshal(obj["capabilities"], &b.Caps)
		if b.Node != expected.NodeID || b.Ledger != expected.LedgerID || b.Boot == "" || len(b.Caps) != 0 {
			done <- ErrProtocol
			return
		}
		sid, _ := protocol.NewID()
		welcome, _ := wireMessage("welcome", sid, env.MessageID, "", map[string]any{"node_id": b.Node, "session_id": sid, "reconcile_required": true})
		p, e := h.Welcome(r.Context(), welcome)
		if e != nil {
			done <- e
			return
		}
		defer p.Close()
		reconcile, _ := wireMessage("reconcile", sid, "", "", map[string]any{"scope": "pending", "scan_id": nil, "cursor": nil, "limit": 200})
		if e = p.Write(r.Context(), reconcile); e != nil {
			done <- e
			return
		}
		_, reply, e := p.Read(r.Context())
		if e != nil || reply.Type != "reconcile_reply" {
			done <- ErrProtocol
			return
		}
		var page struct {
			Scan   string `json:"scan_id"`
			Ledger string `json:"ledger_id"`
			Total  int    `json:"total"`
		}
		json.Unmarshal(reply.Body, &page)
		if page.Ledger != expected.LedgerID || page.Total != 0 {
			done <- ErrProtocol
			return
		}
		complete, _ := wireMessage("reconcile_complete", sid, "", "", map[string]any{"scan_id": page.Scan, "ledger_id": page.Ledger})
		if e = p.Write(r.Context(), complete); e != nil {
			done <- e
			return
		}
		_, reply, e = p.Read(r.Context())
		if e != nil || reply.Type != "reconcile_completed" {
			done <- ErrProtocol
			return
		}
		done <- nil
		_, _, _ = p.Read(r.Context())
	}))
	defer server.Close()
	c.ManagerOrigin = server.URL
	c.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	cert, _ := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	os.WriteFile(c.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600)
	var e error
	expected, e = Initialize(context.Background(), c, "10000000-0000-4000-8000-000000000001")
	if e != nil {
		t.Fatal(e)
	}
	expected.ActiveSecret = expected.CandidateSecret
	expected.CandidateSecret = ""
	expected.CredentialState = "active"
	expected.CredentialGeneration = 1
	if e = saveIdentity(c, expected); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, c) }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
		cancel()
	case <-ctx.Done():
		t.Fatal("recovery did not complete")
	}
	select {
	case e := <-served:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("service did not close")
	}
}
func TestServeAuthenticationRejectionStopsReconnect(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; http.Error(w, "rejected", 401) }))
	defer server.Close()
	c := testConfig(t)
	c.ManagerOrigin = server.URL
	c.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	cert, _ := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	os.WriteFile(c.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600)
	id, e := Initialize(context.Background(), c, "10000000-0000-4000-8000-000000000001")
	if e != nil {
		t.Fatal(e)
	}
	id.ActiveSecret = id.CandidateSecret
	id.CandidateSecret = ""
	id.CredentialState = "active"
	id.CredentialGeneration = 1
	if e = saveIdentity(c, id); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e = Serve(ctx, c); e != ErrUnauthorized {
		t.Fatal("401 did not stop", e)
	}
	if requests != 1 {
		t.Fatal("retried revoked credentials", requests)
	}
}
