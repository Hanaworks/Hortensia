package agenttransport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentservice "docker-manager/internal/agent/service"
	"docker-manager/internal/manager/httpapi"
	"docker-manager/internal/manager/operations"
)

func TestTLSActualAgentServiceEnrollRecoveryAndUnsupportedApply(t *testing.T) {
	f := newTransportPair(t)
	var api http.Handler
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/v1/connect" {
			f.h.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	var err error
	api, err = httpapi.New(httpapi.Config{Store: f.m, ExternalOrigin: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	node := f.service(t, "POST", "/api/v1/nodes", map[string]any{"display_name": "actual service without Docker"}, "")["node_id"].(string)
	f.service(t, "PUT", "/api/v1/nodes/"+node+"/grants/"+f.user, map[string]any{"capabilities": []string{"node.read", "config.read", "config.write", "stack.deploy", "operation.read"}}, `"1"`)
	token := f.service(t, "POST", "/api/v1/nodes/"+node+"/enrollment-tokens", map[string]any{}, "")["token"].(string)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.TLS.Certificates[0].Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	// The CLI deliberately does not exist. No verified version or production
	// capability switch is injected; the service must advertise empty caps.
	config := agentservice.Config{ManagerOrigin: server.URL, CAFile: ca, StateDir: filepath.Join(dir, "state"), IdentityFile: filepath.Join(dir, "identity", "identity.json"), KeysDir: filepath.Join(dir, "keys"), DockerCLI: filepath.Join(dir, "missing-docker"), DockerSocket: "unix:///var/run/docker.sock", ExecutionTimeoutSeconds: 2, LogLevel: "error"}
	identity, err := agentservice.Initialize(f.ctx, config, node)
	if err != nil {
		t.Fatal("actual Agent init", err)
	}
	if err := agentservice.Enroll(f.ctx, config, token); err != nil {
		t.Fatal("actual Agent enroll over HTTPS", err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	served := make(chan error, 1)
	go func() { served <- agentservice.Serve(ctx, config) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Error("actual Agent service shutdown", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("actual Agent service did not stop")
		}
	})
	transportEventually(t, f.ctx, "actual Agent/Manager did not finish recovery", func() bool {
		view := f.service(t, "GET", "/api/v1/nodes/"+node, nil, "")
		return view["status"] == "online"
	})
	f.h.mu.Lock()
	connection := f.h.connections[node]
	if connection == nil {
		f.h.mu.Unlock()
		t.Fatal("actual Agent is not registered")
	}
	binding := connection.b
	f.h.mu.Unlock()
	if binding.NodeID != identity.NodeID || binding.LedgerID != identity.LedgerID {
		t.Fatal("service hello did not use initialized durable identity")
	}
	state, err := f.m.ReadRuntimeConnectionState(f.ctx, binding)
	if err != nil || state.Frozen || state.Recovery.Status != "completed" || state.Recovery.Seen != 0 || state.Recovery.Total != 0 {
		t.Fatalf("actual recovery was not an empty completed scan: %+v %v", state, err)
	}
	view := f.service(t, "GET", "/api/v1/nodes/"+node, nil, "")
	caps, ok := view["agent_capabilities"].([]any)
	if !ok || len(caps) != 0 {
		t.Fatal("service without Docker advertised mutation capabilities", view["agent_capabilities"])
	}
	bundle := map[string]any{"files": []any{map[string]any{"path": "compose.yaml", "content_b64": base64.StdEncoding.EncodeToString([]byte("services: {}\n"))}}, "compose_files": []string{"compose.yaml"}, "interpolation_env_files": []string{}, "interpolation_env": map[string]string{}, "profiles": []string{}}
	created := f.service(t, "POST", "/api/v1/nodes/"+node+"/stacks", map[string]any{"project_name": "actual_service_project", "config": bundle}, "")
	raw, _ := json.Marshal(map[string]any{"version_id": created["version_id"]})
	r, err := http.NewRequestWithContext(f.ctx, "POST", server.URL+"/api/v1/stacks/"+created["stack_id"].(string)+"/apply", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", server.URL)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", reply(t, f.login.Reply)["csrf_token"].(string))
	r.Header.Set("Idempotency-Key", id(t))
	r.AddCookie(&http.Cookie{Name: "__Host-dm_session", Value: f.login.Token})
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var rejected struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rejected); err != nil || response.StatusCode != 422 || rejected.Error.Code != "unsupported" {
		t.Fatalf("unavailable runtime Apply status=%d body=%s error=%v", response.StatusCode, body, err)
	}
	listed, err := f.m.DoService(f.ctx, f.login.Session, operations.ServiceRequest{Method: "GET", Path: "/api/v1/operations", NodeID: node})
	if err != nil {
		t.Fatal(err)
	}
	operationList := reply(t, listed)
	if items, ok := operationList["items"].([]any); !ok || len(items) != 0 {
		t.Fatal("unsupported Apply created an operation", operationList)
	}
}
