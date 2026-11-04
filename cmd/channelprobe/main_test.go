package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testNode = "11111111-1111-4111-8111-111111111111"
const testLedger = "22222222-2222-4222-8222-222222222222"
const testToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// Removing argument validation or allowing flag errors to echo arbitrary values
// must fail these tests before a listener or connection can be opened.
func TestRejectArgumentsWithoutLeakingSecrets(t *testing.T) {
	secret := "do-not-print-this-secret"
	for _, args := range [][]string{
		nil,
		{"-mode", secret},
		{"-mode", "agent", "-duration", secret},
		{"-mode", "agent", "-duration", "0s"},
		{"-mode", "agent", "-duration", "-1s"},
		{"-unknown-" + secret},
		{"-mode", "agent", secret},
		{"-mode", "agent", "-url", "ws://127.0.0.1/agent/v1/connect"},
		{"-mode", "manager", "-listen", "0.0.0.0:8443"},
		{"-mode", "manager", "-listen", ":8443"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			if code := run(context.Background(), args, &out, &diagnostic); code == 0 {
				t.Fatal("invalid arguments succeeded")
			}
			if out.Len() != 0 || strings.Contains(diagnostic.String(), secret) {
				t.Fatalf("argument output leaked input: stdout=%q stderr=%q", out.String(), diagnostic.String())
			}
		})
	}
}

func TestTokenErrorsNeverEchoFileContentsOrPath(t *testing.T) {
	secret := "private-invalid-token"
	path := filepath.Join(t.TempDir(), "secret-file-name")
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-mode", "agent", "-url", "wss://127.0.0.1:8443/agent/v1/connect", "-node-id", testNode, "-ledger-id", testLedger, "-token-file", path}
	var out, diagnostic bytes.Buffer
	if code := run(context.Background(), args, &out, &diagnostic); code == 0 {
		t.Fatal("invalid token succeeded")
	}
	if out.Len() != 0 || strings.Contains(diagnostic.String(), secret) || strings.Contains(diagnostic.String(), path) {
		t.Fatalf("token input leaked: stdout=%q stderr=%q", out.String(), diagnostic.String())
	}
	if strings.TrimSpace(diagnostic.String()) != "invalid_token_file" {
		t.Fatalf("unexpected diagnostic: %q", diagnostic.String())
	}
}
