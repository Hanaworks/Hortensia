package service

import (
	"context"
	"crypto/x509"
	"docker-manager/internal/agentwire"
	"docker-manager/internal/protocol"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCredentialLostRepliesReuseDurableCandidate(t *testing.T) {
	var mu sync.Mutex
	active := ""
	enrolls, rotates := 0, 0
	var server *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		current := active
		mu.Unlock()
		if r.URL.Path == "/agent/v1/connect" {
			if token != current || current == "" {
				http.Error(w, "rejected", 401)
				return
			}
			h, e := agentwire.Accept(w, r)
			if e != nil {
				return
			}
			defer h.Close()
			raw, e := h.ReadHello(r.Context())
			if e != nil {
				return
			}
			v, _ := protocol.NewValidator()
			hello, e := v.Validate(raw)
			if e != nil {
				return
			}
			var b struct {
				Node string `json:"node_id"`
			}
			json.Unmarshal(hello.Body, &b)
			sid, _ := protocol.NewID()
			welcome, _ := wireMessage("welcome", sid, hello.MessageID, "", map[string]any{"node_id": b.Node, "session_id": sid, "reconcile_required": true})
			p, e := h.Welcome(r.Context(), welcome)
			if e != nil {
				return
			}
			defer p.Close()
			_, _, _ = p.Read(r.Context())
			return
		}
		var b struct {
			Secret string `json:"credential_secret"`
		}
		if json.NewDecoder(r.Body).Decode(&b) != nil || !validSecret(b.Secret) {
			http.Error(w, "bad", 400)
			return
		}
		mu.Lock()
		switch r.URL.Path {
		case "/agent/v1/enroll":
			if active != "" {
				mu.Unlock()
				http.Error(w, "used", 401)
				return
			}
			enrolls++
			active = b.Secret
		case "/agent/v1/credentials/rotate":
			if token != active {
				mu.Unlock()
				http.Error(w, "old", 401)
				return
			}
			rotates++
			active = b.Secret
		default:
			mu.Unlock()
			http.NotFound(w, r)
			return
		}
		mu.Unlock()
		conn, _, e := w.(http.Hijacker).Hijack()
		if e == nil {
			conn.Close()
		}
	})
	server = httptest.NewTLSServer(handler)
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
	token, _ := newSecret()
	if e = Enroll(context.Background(), c, token); !errors.Is(e, ErrTransport) {
		t.Fatal("lost enroll reply", e)
	}
	pending, k, e := loadIdentity(c)
	clearKeys(&k)
	if e != nil || pending.CandidateSecret != id.CandidateSecret || pending.CredentialState != "candidate" {
		t.Fatal("candidate changed after lost response")
	}
	if e = Enroll(context.Background(), c, token); e != nil {
		t.Fatal(e)
	}
	if e = RotateCredentials(context.Background(), c); !errors.Is(e, ErrTransport) {
		t.Fatal("lost rotation reply", e)
	}
	pending, k, e = loadIdentity(c)
	clearKeys(&k)
	candidate := pending.CandidateSecret
	if e != nil || pending.CredentialState != "rotation_pending" {
		t.Fatal("rotation not durable")
	}
	if e = RotateCredentials(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	id, k, e = loadIdentity(c)
	clearKeys(&k)
	mu.Lock()
	defer mu.Unlock()
	if e != nil || id.ActiveSecret != candidate || id.CredentialGeneration != 2 || id.LedgerID != pending.LedgerID || enrolls != 1 || rotates != 1 {
		t.Fatal("did not reuse same candidate and ledger")
	}
}
