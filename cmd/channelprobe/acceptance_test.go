package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"docker-manager/internal/channel"
)

// Each rejection starts with working TLS/identity materials and changes one
// input. Removing a check, choosing the wrong exit class, or echoing an input
// must fail the assertions, rather than pass because another input is invalid.
type acceptanceFiles struct{ cert, key, token string }

func newAcceptanceFiles(t *testing.T) acceptanceFiles {
	t.Helper()
	cert, key, token, _ := makeTLSFiles(t)
	return acceptanceFiles{cert, key, token}
}

func (f acceptanceFiles) args(mode, endpoint string) []string {
	args := []string{"-mode", mode, "-node-id", testNode, "-ledger-id", testLedger, "-token-file", f.token, "-duration", "2s"}
	if mode == "manager" {
		return append(args, "-listen", "127.0.0.1:0", "-cert", f.cert, "-key", f.key)
	}
	return append(args, "-url", endpoint, "-ca", f.cert)
}

func replaceArg(args []string, flag, value string) []string {
	args = append([]string(nil), args...)
	for i := range args {
		if args[i] == flag {
			args[i+1] = value
			return args
		}
	}
	return append(args, flag, value)
}

func acceptanceFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private-material-do-not-echo")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The real channel handler and a real verified TLS connection are retained.
// Counting requests also detects unwanted retries and premature network access.
func acceptanceServer(t *testing.T, f acceptanceFiles, handler http.Handler) (string, *atomic.Int32) {
	t.Helper()
	if handler == nil {
		s, err := channel.NewServer(channel.Config{NodeID: testNode, LedgerID: testLedger, Token: testToken})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		handler = s
	}
	pair, err := tls.LoadX509KeyPair(f.cert, f.key)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	t.Cleanup(s.Close)
	return "wss://" + s.Listener.Addr().String() + "/agent/v1/connect", &requests
}

func requireCLIResult(t *testing.T, code, wantCode int, out, diagnostic *bytes.Buffer, wantDiagnostic string) {
	t.Helper()
	if code != wantCode || diagnostic.String() != wantDiagnostic {
		t.Fatalf("exit=%d stderr=%q; want exit=%d stderr=%q", code, diagnostic.String(), wantCode, wantDiagnostic)
	}
	if wantCode != 0 && out.Len() != 0 {
		t.Fatalf("unexpected stdout before successful startup: %q", out.String())
	}
}

func requireNoSecrets(t *testing.T, f acceptanceFiles, output string, additional ...string) {
	t.Helper()
	key, err := os.ReadFile(f.key)
	if err != nil {
		t.Fatal(err)
	}
	secrets := append([]string{testToken, f.token, f.key, f.cert, string(key), "PRIVATE KEY"}, additional...)
	for _, secret := range secrets {
		if secret != "" && strings.Contains(output, secret) {
			t.Fatal("CLI output leaked a credential, private material, path, or rejected input")
		}
	}
}

func TestCLIArgumentDiagnostics(t *testing.T) {
	f := newAcceptanceFiles(t)
	endpoint, requests := acceptanceServer(t, f, nil)
	manager, agent := f.args("manager", ""), f.args("agent", endpoint)
	secret := "rejected-input-do-not-echo"
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing_mode", replaceArg(manager, "-mode", ""), "invalid_arguments\n"},
		{"unknown_mode", replaceArg(manager, "-mode", secret), "invalid_arguments\n"},
		{"invalid_duration", replaceArg(manager, "-duration", secret), "invalid_arguments\n"},
		{"zero_duration", replaceArg(manager, "-duration", "0s"), "invalid_arguments\n"},
		{"negative_duration", replaceArg(manager, "-duration", "-1s"), "invalid_arguments\n"},
		{"duration_overflow", replaceArg(manager, "-duration", "999999999999999999999h"), "invalid_arguments\n"},
		{"unknown_flag", append(append([]string{}, manager...), "-"+secret), "invalid_arguments\n"},
		{"missing_flag_value", append(append([]string{}, manager...), "-duration"), "invalid_arguments\n"},
		{"positional_argument", append(append([]string{}, manager...), secret), "invalid_arguments\n"},
		{"help", append(append([]string{}, manager...), "-h"), "invalid_arguments\n"},
		{"missing_cert", replaceArg(manager, "-cert", ""), "tls_files_required\n"},
		{"missing_key", replaceArg(manager, "-key", ""), "tls_files_required\n"},
		{"missing_url", replaceArg(agent, "-url", ""), "invalid_agent_url\n"},
	}
	for _, mode := range []string{"manager", "agent"} {
		for _, flag := range []string{"-node-id", "-ledger-id", "-token-file"} {
			tests = append(tests, struct {
				name string
				args []string
				want string
			}{mode + "_missing_" + flag, replaceArg(f.args(mode, endpoint), flag, ""), "identity_files_required\n"})
		}
	}
	for _, addr := range []string{"localhost:0", "0.0.0.0:0", "[::]:0", ":0", "192.0.2.1:0", "127.0.0.1", "127.0.0.1:", "127.0.0.1:-1", "127.0.0.1:65536", "127.0.0.1:http", "[::1%zone]:0", secret} {
		tests = append(tests, struct {
			name string
			args []string
			want string
		}{"listen_" + addr, replaceArg(manager, "-listen", addr), "loopback_listen_required\n"})
	}
	for name, value := range map[string]string{
		"plain_ws":       strings.Replace(endpoint, "wss:", "ws:", 1),
		"https":          strings.Replace(endpoint, "wss:", "https:", 1),
		"missing_host":   "wss:///agent/v1/connect",
		"userinfo":       strings.Replace(endpoint, "wss://", "wss://"+secret+"@", 1),
		"query":          endpoint + "?token=" + secret,
		"empty_query":    endpoint + "?",
		"fragment":       endpoint + "#" + secret,
		"wrong_path":     strings.Replace(endpoint, "/agent/v1/connect", "/"+secret, 1),
		"trailing_slash": endpoint + "/",
		"bad_escape":     endpoint + "%zz",
	} {
		tests = append(tests, struct {
			name string
			args []string
			want string
		}{"url_" + name, replaceArg(agent, "-url", value), "invalid_agent_url\n"})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			code := run(context.Background(), tt.args, &out, &diagnostic)
			requireCLIResult(t, code, 2, &out, &diagnostic, tt.want)
			requireNoSecrets(t, f, out.String()+diagnostic.String(), secret)
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid arguments reached server %d times", requests.Load())
	}
}

func TestCLIListenPortBoundsAndDefaults(t *testing.T) {
	f := newAcceptanceFiles(t)
	for _, addr := range []string{"127.0.0.1:0", "127.0.0.1:65535", "[::1]:0", "[::1]:65535"} {
		o, problem := parse(replaceArg(f.args("manager", ""), "-listen", addr))
		if problem != "" || o.listen != addr {
			t.Fatalf("valid loopback port boundary %q rejected: %s", addr, problem)
		}
	}
	o, problem := parse([]string{"-mode", "manager", "-cert", f.cert, "-key", f.key, "-token-file", f.token, "-node-id", testNode, "-ledger-id", testLedger})
	if problem != "" || o.duration != 30*time.Second || o.listen != "127.0.0.1:8443" {
		t.Fatalf("default run options: %+v problem=%s", o, problem)
	}
}

// Replacing strict canonical token validation with loose Base64 decoding or
// whitespace trimming must fail, even though all remaining materials are valid.
func TestCLITokenFileRejections(t *testing.T) {
	f := newAcceptanceFiles(t)
	endpoint, requests := acceptanceServer(t, f, nil)
	invalid := map[string]string{
		"empty": "", "42_characters": testToken[:42], "44_characters": testToken + "A",
		"padding": testToken + "=", "noncanonical_pad_bits": testToken[:42] + "B",
		"standard_base64_plus": "+" + testToken[1:], "standard_base64_slash": "/" + testToken[1:],
		"leading_space": " " + testToken, "trailing_space": testToken + " ", "tab": testToken + "\t",
		"embedded_lf": testToken[:20] + "\n" + testToken[20:],
		"two_lf":      testToken + "\n\n", "two_cr": testToken + "\r\r", "two_crlf": testToken + "\r\n\r\n",
		"lfcr": testToken + "\n\r", "second_line": testToken + "\nprivate-second-line",
		"unicode": testToken[:42] + "密", "nul": testToken + "\x00",
	}
	for _, mode := range []string{"manager", "agent"} {
		for name, contents := range invalid {
			t.Run(mode+"/"+name, func(t *testing.T) {
				path := acceptanceFile(t, contents)
				var out, diagnostic bytes.Buffer
				code := run(context.Background(), replaceArg(f.args(mode, endpoint), "-token-file", path), &out, &diagnostic)
				requireCLIResult(t, code, 2, &out, &diagnostic, "invalid_token_file\n")
				requireNoSecrets(t, f, out.String()+diagnostic.String(), contents, path)
			})
		}
		for name, path := range map[string]string{"missing": filepath.Join(t.TempDir(), "missing-secret-token"), "directory": t.TempDir()} {
			t.Run(mode+"/"+name, func(t *testing.T) {
				var out, diagnostic bytes.Buffer
				code := run(context.Background(), replaceArg(f.args(mode, endpoint), "-token-file", path), &out, &diagnostic)
				requireCLIResult(t, code, 2, &out, &diagnostic, "invalid_token_file\n")
				requireNoSecrets(t, f, out.String()+diagnostic.String(), path)
			})
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid token reached server %d times", requests.Load())
	}
}

func TestCLIIdentityDiagnosticsDependOnMode(t *testing.T) {
	f := newAcceptanceFiles(t)
	endpoint, requests := acceptanceServer(t, f, nil)
	for _, mode := range []string{"manager", "agent"} {
		for _, flag := range []string{"-node-id", "-ledger-id"} {
			for name, id := range map[string]string{
				"uppercase":     "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA",
				"wrong_version": "11111111-1111-3111-8111-111111111111",
				"wrong_variant": "11111111-1111-4111-7111-111111111111",
				"short":         testNode[:35], "extra": testNode + "1", "space": testNode + " ", "raw_secret": "invalid-identity-secret",
			} {
				t.Run(mode+"/"+flag+"/"+name, func(t *testing.T) {
					var out, diagnostic bytes.Buffer
					code := run(context.Background(), replaceArg(f.args(mode, endpoint), flag, id), &out, &diagnostic)
					wantCode, want := 2, "invalid_identity\n"
					if mode == "agent" {
						wantCode, want = 1, "connection_failed\n"
					}
					requireCLIResult(t, code, wantCode, &out, &diagnostic, want)
					requireNoSecrets(t, f, out.String()+diagnostic.String(), id)
				})
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid UUID opened %d connections", requests.Load())
	}
}

func TestCLITLSMaterialDiagnostics(t *testing.T) {
	f := newAcceptanceFiles(t)
	other := newAcceptanceFiles(t)
	endpoint, requests := acceptanceServer(t, f, nil)
	bad := acceptanceFile(t, "private-invalid-PEM-material")
	empty := acceptanceFile(t, "")
	missing := filepath.Join(t.TempDir(), "missing-private-material")
	directory := t.TempDir()
	for _, flag := range []string{"-cert", "-key", "-ca"} {
		mode, want := "manager", "invalid_tls_files\n"
		if flag == "-ca" {
			mode, want = "agent", "invalid_ca_file\n"
		}
		values := map[string]string{"missing": missing, "malformed": bad, "empty": empty, "directory": directory}
		if flag == "-cert" {
			values["mismatch"] = other.cert
			values["key_instead_of_cert"] = f.key
		}
		if flag == "-key" {
			values["mismatch"] = other.key
			values["cert_instead_of_key"] = f.cert
		}
		if flag == "-ca" {
			values["key_instead_of_ca"] = f.key
		}
		for name, path := range values {
			t.Run(flag+"/"+name, func(t *testing.T) {
				var out, diagnostic bytes.Buffer
				code := run(context.Background(), replaceArg(f.args(mode, endpoint), flag, path), &out, &diagnostic)
				requireCLIResult(t, code, 2, &out, &diagnostic, want)
				requireNoSecrets(t, f, out.String()+diagnostic.String(), path, "private-invalid-PEM-material")
			})
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid TLS materials reached server %d times", requests.Load())
	}
}

type acceptanceWriter struct {
	bytes.Buffer
	afterWrite func()
	failure    error
	writes     int
}

func (w *acceptanceWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failure != nil {
		return 0, w.failure
	}
	n, err := w.Buffer.Write(p)
	if w.afterWrite != nil {
		w.afterWrite()
	}
	return n, err
}

func requireReport(t *testing.T, raw, mode string) map[string]any {
	t.Helper()
	if strings.Count(raw, "\n") != 1 || !strings.HasSuffix(raw, "\n") {
		t.Fatalf("not one JSON line: %q", raw)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	if report["node_id"] != testNode || report["ledger_id"] != testLedger {
		t.Fatalf("wrong identity report: %v", report)
	}
	if mode == "manager" {
		if len(report) != 4 || report["event"] != "listening" {
			t.Fatalf("wrong listening report fields: %v", report)
		}
		addr, ok := report["address"].(string)
		if !ok {
			t.Fatal("address missing")
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil || host != "127.0.0.1" || port == "" || port == "0" {
			t.Fatalf("invalid allocated address %q", addr)
		}
	} else {
		id, ok := report["session_id"].(string)
		if len(report) != 5 || report["event"] != "handshake" || report["reconcile_required"] != true || !ok || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
			t.Fatalf("wrong handshake report: %v", report)
		}
	}
	return report
}

func TestCLIAcceptsCanonicalTokensAndSingleLineEndings(t *testing.T) {
	f := newAcceptanceFiles(t)
	endpoint, _ := acceptanceServer(t, f, nil)
	for _, mode := range []string{"manager", "agent"} {
		for name, ending := range map[string]string{"none": "", "LF": "\n", "CR": "\r", "CRLF": "\r\n"} {
			t.Run(mode+"/"+name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				out := &acceptanceWriter{afterWrite: cancel}
				var diagnostic bytes.Buffer
				args := replaceArg(f.args(mode, endpoint), "-token-file", acceptanceFile(t, testToken+ending))
				code := run(ctx, args, out, &diagnostic)
				requireCLIResult(t, code, 0, &out.Buffer, &diagnostic, "")
				requireReport(t, out.String(), mode)
				requireNoSecrets(t, f, out.String()+diagnostic.String())
			})
		}
	}
}

func TestCLIIgnoresOtherModeOptions(t *testing.T) {
	f := newAcceptanceFiles(t)
	endpoint, _ := acceptanceServer(t, f, nil)
	for _, mode := range []string{"manager", "agent"} {
		t.Run(mode, func(t *testing.T) {
			args := f.args(mode, endpoint)
			if mode == "manager" {
				args = append(args, "-url", "not-a-url", "-ca", "missing-unused-CA")
			} else {
				args = append(args, "-listen", "not-a-listener", "-cert", "missing-unused-cert", "-key", "missing-unused-key")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out := &acceptanceWriter{afterWrite: cancel}
			var diagnostic bytes.Buffer
			code := run(ctx, args, out, &diagnostic)
			requireCLIResult(t, code, 0, &out.Buffer, &diagnostic, "")
			requireReport(t, out.String(), mode)
		})
	}
}

func TestCLIAcceptsURLSafeAlphabetAndEmptyFragment(t *testing.T) {
	f := newAcceptanceFiles(t)
	token := strings.Repeat("-_", 21) + "A" // 43 canonical Base64url characters.
	f.token = acceptanceFile(t, token)
	handler, err := channel.NewServer(channel.Config{NodeID: testNode, LedgerID: testLedger, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	endpoint, _ := acceptanceServer(t, f, handler)
	for _, mode := range []string{"manager", "agent"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out := &acceptanceWriter{afterWrite: cancel}
			var diagnostic bytes.Buffer
			code := run(ctx, f.args(mode, endpoint+"#"), out, &diagnostic)
			requireCLIResult(t, code, 0, &out.Buffer, &diagnostic, "")
			requireReport(t, out.String(), mode)
			requireNoSecrets(t, f, out.String()+diagnostic.String(), token)
		})
	}
}

func TestCLIRunningDurationExpiryIsSuccessful(t *testing.T) {
	f := newAcceptanceFiles(t)
	endpoint, _ := acceptanceServer(t, f, nil)
	for _, mode := range []string{"manager", "agent"} {
		t.Run(mode, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			started := time.Now()
			code := run(context.Background(), replaceArg(f.args(mode, endpoint), "-duration", "200ms"), &out, &diagnostic)
			elapsed := time.Since(started)
			requireCLIResult(t, code, 0, &out, &diagnostic, "")
			requireReport(t, out.String(), mode)
			if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
				t.Fatalf("duration exit took %v", elapsed)
			}
		})
	}
}

func TestCLIConnectingDurationExpiryIsFailure(t *testing.T) {
	f := newAcceptanceFiles(t)
	entered := make(chan struct{}, 1)
	endpoint, requests := acceptanceServer(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-r.Context().Done() // Trusted TLS succeeded; deliberately withhold Upgrade.
	}))
	var out, diagnostic bytes.Buffer
	started := time.Now()
	code := run(context.Background(), replaceArg(f.args("agent", endpoint), "-duration", "200ms"), &out, &diagnostic)
	elapsed := time.Since(started)
	requireCLIResult(t, code, 1, &out, &diagnostic, "connection_failed\n")
	if len(entered) != 1 || requests.Load() != 1 {
		t.Fatalf("connection stage not reached exactly once: %d", requests.Load())
	}
	if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("connect duration exit took %v", elapsed)
	}
}

func TestCLIConnectionFailuresDoNotRetryOrLeak(t *testing.T) {
	f := newAcceptanceFiles(t)
	endpoint, requests := acceptanceServer(t, f, nil)
	other := newAcceptanceFiles(t)
	wrongToken := strings.Repeat("B", 42) + "A" // Canonical encoding of exactly 32 bytes.
	for _, tt := range []struct {
		name, flag, value, diagnostic string
		requests                      int32
	}{
		{"401", "-token-file", acceptanceFile(t, wrongToken), "unauthorized\n", 1},
		{"node_mismatch", "-node-id", "33333333-3333-4333-8333-333333333333", "connection_failed\n", 1},
		{"ledger_mismatch", "-ledger-id", "33333333-3333-4333-8333-333333333333", "connection_failed\n", 1},
		{"untrusted_CA", "-ca", other.cert, "connection_failed\n", 0},
		// This only checks CLI failure mapping: localhost may fail before TLS.
		// channel.TestTLSAndIdentityRejection forces ServerName on a known endpoint.
		{"hostname_url_failure_mapping", "-url", strings.Replace(endpoint, "127.0.0.1", "localhost", 1), "connection_failed\n", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := requests.Load()
			var out, diagnostic bytes.Buffer
			code := run(context.Background(), replaceArg(f.args("agent", endpoint), tt.flag, tt.value), &out, &diagnostic)
			requireCLIResult(t, code, 1, &out, &diagnostic, tt.diagnostic)
			if got := requests.Load() - before; got != tt.requests {
				t.Fatalf("requests=%d want=%d (unexpected retry or TLS path)", got, tt.requests)
			}
			requireNoSecrets(t, f, out.String()+diagnostic.String(), wrongToken, tt.value)
		})
	}
}

func TestCLIDisconnectAfterHandshakeIsFailure(t *testing.T) {
	f := newAcceptanceFiles(t)
	handler, err := channel.NewServer(channel.Config{NodeID: testNode, LedgerID: testLedger, Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	endpoint, requests := acceptanceServer(t, f, handler)
	out := &acceptanceWriter{afterWrite: handler.Close}
	var diagnostic bytes.Buffer
	code := run(context.Background(), f.args("agent", endpoint), out, &diagnostic)
	if code != 1 || diagnostic.String() != "connection_closed\n" {
		t.Fatalf("disconnect exit=%d stderr=%q", code, diagnostic.String())
	}
	requireReport(t, out.String(), "agent")
	if requests.Load() != 1 {
		t.Fatalf("agent retried after disconnect: %d requests", requests.Load())
	}
	requireNoSecrets(t, f, out.String()+diagnostic.String())
}

func TestCLIListenConflictIsRuntimeFailure(t *testing.T) {
	f := newAcceptanceFiles(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var out, diagnostic bytes.Buffer
	code := run(context.Background(), replaceArg(f.args("manager", ""), "-listen", listener.Addr().String()), &out, &diagnostic)
	requireCLIResult(t, code, 1, &out, &diagnostic, "listen_failed\n")
	requireNoSecrets(t, f, out.String()+diagnostic.String(), listener.Addr().String())
}

func TestCLIReportFailureIsRuntimeFailure(t *testing.T) {
	f := newAcceptanceFiles(t)
	endpoint, _ := acceptanceServer(t, f, nil)
	for _, mode := range []string{"manager", "agent"} {
		t.Run(mode, func(t *testing.T) {
			out := &acceptanceWriter{failure: errors.New("private-writer-failure-do-not-echo")}
			var diagnostic bytes.Buffer
			started := time.Now()
			code := run(context.Background(), f.args(mode, endpoint), out, &diagnostic)
			requireCLIResult(t, code, 1, &out.Buffer, &diagnostic, "report_failed\n")
			if out.writes != 1 {
				t.Fatalf("report attempted %d writes", out.writes)
			}
			if time.Since(started) > time.Second {
				t.Fatal("report failure did not promptly terminate")
			}
			requireNoSecrets(t, f, diagnostic.String(), "private-writer-failure-do-not-echo")
		})
	}
}
