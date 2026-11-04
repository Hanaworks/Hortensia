package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run the real command entry logic in a separate OS process. Closing the
// control pipe cancels its context, so tests can shut down without signals or sleeps.
func TestProbeProcess(t *testing.T) {
	if os.Getenv("CHANNELPROBE_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
		code := run(ctx, os.Args[i+1:], os.Stdout, os.Stderr)
		cancel()
		os.Exit(code)
	}
	os.Exit(99)
}

type probeProcess struct {
	input              io.WriteCloser
	lines              <-chan string
	done               <-chan error
	output, diagnostic *bytes.Buffer
	waited             bool
}

func launchProbe(t *testing.T, ctx context.Context, args ...string) *probeProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestProbeProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "CHANNELPROBE_HELPER=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output, diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 8)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		defer close(lines)
		s := bufio.NewScanner(stdout)
		for s.Scan() {
			line := s.Text()
			output.WriteString(line + "\n")
			select {
			case lines <- line:
			case <-ctx.Done():
			}
		}
	}()
	done := make(chan error, 1)
	go func() { <-scanned; done <- cmd.Wait() }()
	p := &probeProcess{input: input, lines: lines, done: done, output: &output, diagnostic: &diagnostic}
	t.Cleanup(func() {
		_ = p.input.Close()
		if !p.waited {
			<-p.done
			p.waited = true
		}
	})
	return p
}

func (p *probeProcess) line(t *testing.T, ctx context.Context) map[string]any {
	t.Helper()
	select {
	case raw, ok := <-p.lines:
		if !ok {
			t.Fatal("probe exited without readiness/handshake JSON")
		}
		var report map[string]any
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			t.Fatalf("invalid report: %q", raw)
		}
		return report
	case <-ctx.Done():
		t.Fatal("probe report deadline exceeded")
	}
	return nil
}

func (p *probeProcess) wait(t *testing.T, success bool) {
	t.Helper()
	err := <-p.done
	p.waited = true
	if (err == nil) != success {
		t.Fatalf("process exit=%v stderr=%q stdout=%q", err, p.diagnostic.String(), p.output.String())
	}
}

func makeTLSFiles(t *testing.T) (certPath, keyPath, tokenPath string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "channelprobe-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath, tokenPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), filepath.Join(dir, "token")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for path, data := range map[string][]byte{
		certPath:  certPEM,
		keyPath:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		tokenPath: []byte(testToken + "\n"),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	pool = x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("invalid generated certificate")
	}
	return
}

// These assertions exercise OS process boundaries, certificate verification,
// negotiated TLS, protocol identity, duration expiry, and graceful cleanup.
func TestManagerAndAgentProcessesUseVerifiedTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cert, key, token, pool := makeTLSFiles(t)
	manager := launchProbe(t, ctx, "-mode", "manager", "-listen", "127.0.0.1:0", "-cert", cert, "-key", key, "-token-file", token, "-node-id", testNode, "-ledger-id", testLedger, "-duration", "15s")
	ready := manager.line(t, ctx)
	addr, ok := ready["address"].(string)
	if !ok || ready["event"] != "listening" {
		t.Fatalf("unexpected ready report: %v", ready)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("real TLS handshake failed: %v", err)
	}
	state := conn.ConnectionState()
	_ = conn.Close()
	if !state.HandshakeComplete || state.Version < tls.VersionTLS12 || len(state.VerifiedChains) == 0 {
		t.Fatal("TLS verification missing")
	}
	oldTLS, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS11, MaxVersion: tls.VersionTLS11})
	if err == nil {
		_ = oldTLS.Close()
		t.Fatal("manager accepted TLS 1.1")
	}

	base := []string{"-mode", "agent", "-url", "wss://" + addr + "/agent/v1/connect", "-node-id", testNode, "-ledger-id", testLedger, "-duration", "2s"}
	agent := launchProbe(t, ctx, append(append([]string{}, base...), "-token-file", token, "-ca", cert)...)
	handshake := agent.line(t, ctx)
	if handshake["event"] != "handshake" || handshake["node_id"] != testNode || handshake["ledger_id"] != testLedger || handshake["reconcile_required"] != true || handshake["session_id"] == "" || handshake["session_id"] == nil {
		t.Fatalf("unexpected handshake: %v", handshake)
	}
	for field := range handshake {
		switch field {
		case "event", "node_id", "ledger_id", "session_id", "reconcile_required":
		default:
			t.Fatalf("unexpected output field %q", field)
		}
	}
	agent.wait(t, true)
	if agent.diagnostic.Len() != 0 {
		t.Fatalf("duration expiry was not clean: %q", agent.diagnostic.String())
	}

	untrusted := launchProbe(t, ctx, append(append([]string{}, base...), "-token-file", token)...)
	untrusted.wait(t, false)
	if strings.TrimSpace(untrusted.diagnostic.String()) != "connection_failed" || untrusted.output.Len() != 0 {
		t.Fatal("untrusted TLS was not rejected with a fixed diagnostic")
	}
	wrongToken := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	wrongFile := filepath.Join(t.TempDir(), "wrong-token")
	if err := os.WriteFile(wrongFile, []byte(wrongToken), 0600); err != nil {
		t.Fatal(err)
	}
	unauthorized := launchProbe(t, ctx, append(append([]string{}, base...), "-token-file", wrongFile, "-ca", cert)...)
	unauthorized.wait(t, false)
	if strings.TrimSpace(unauthorized.diagnostic.String()) != "unauthorized" || unauthorized.output.Len() != 0 {
		t.Fatal("401 did not terminate with fixed diagnostic")
	}

	_ = manager.input.Close()
	manager.wait(t, true)
	for _, p := range []*probeProcess{manager, agent, untrusted, unauthorized} {
		if strings.Contains(p.output.String()+p.diagnostic.String(), testToken) || strings.Contains(p.output.String()+p.diagnostic.String(), wrongToken) {
			t.Fatal("credential leaked")
		}
	}
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("manager listener remained open")
	}
}

func TestManagerRejectsNonLoopbackBeforeOpeningListener(t *testing.T) {
	cert, key, token, _ := makeTLSFiles(t)
	for _, addr := range []string{"0.0.0.0:0", "[::]:0", ":0", "192.0.2.1:0", "localhost:0"} {
		var out, diagnostic bytes.Buffer
		code := run(context.Background(), []string{"-mode", "manager", "-listen", addr, "-cert", cert, "-key", key, "-token-file", token, "-node-id", testNode, "-ledger-id", testLedger}, &out, &diagnostic)
		if code == 0 || out.Len() != 0 || strings.TrimSpace(diagnostic.String()) != "loopback_listen_required" {
			t.Fatalf("unsafe listen accepted: %s", addr)
		}
	}
}
