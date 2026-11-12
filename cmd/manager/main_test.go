package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func commandConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	config := map[string]string{"external_origin": "https://localhost:8443", "tls_cert_file": filepath.Join(dir, "cert.pem"), "tls_key_file": filepath.Join(dir, "key.pem"), "database_file": filepath.Join(dir, "data", "manager.db"), "keys_dir": filepath.Join(dir, "keys"), "web_dir": filepath.Join(dir, "web")}
	raw, _ := json.Marshal(config)
	file := filepath.Join(dir, "manager.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func commandInput(t *testing.T, value string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(value); err != nil {
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { r.Close() })
	return r
}

func TestInitCommandRequiresExplicitPipeAndPreservesExisting(t *testing.T) {
	config := commandConfig(t)
	var out, diagnostic bytes.Buffer
	args := []string{"init", "--config", config, "--admin", "admin"}
	if code := run(context.Background(), args, commandInput(t, "test password value\n"), &out, &diagnostic); code == 0 {
		t.Fatal("pipe silently accepted without explicit option")
	}
	args = append(args, "--password-stdin")
	out.Reset()
	diagnostic.Reset()
	if code := run(context.Background(), args, commandInput(t, "test password value\n"), &out, &diagnostic); code != 0 {
		t.Fatalf("init failed %d %s", code, &diagnostic)
	}
	if !strings.Contains(out.String(), "已初始化") || strings.Contains(out.String()+diagnostic.String(), "test password value") {
		t.Fatal("missing outcome or secret in diagnostic")
	}
	out.Reset()
	diagnostic.Reset()
	if code := run(context.Background(), args, commandInput(t, "different password\n"), &out, &diagnostic); code == 0 || !strings.Contains(diagnostic.String(), "initialization_materials_exist") {
		t.Fatal("repeat init did not preserve identity")
	}
}

func TestCommandNeverEchoesRejectedArguments(t *testing.T) {
	var out, diagnostic bytes.Buffer
	code := run(context.Background(), []string{"init", "--password", "do-not-echo-me"}, commandInput(t, ""), &out, &diagnostic)
	if code != 2 || strings.Contains(diagnostic.String(), "do-not-echo-me") {
		t.Fatal("argument parser leaked input")
	}
}

func TestReviewCanceledInitializationDoesNotWaitForPassword(t *testing.T) {
	config := commandConfig(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	var out, diagnostic bytes.Buffer
	go func() {
		done <- run(ctx, []string{"init", "--config", config, "--admin", "admin", "--password-stdin"}, reader, &out, &diagnostic)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		writer.Close()
		<-done
		t.Fatal("canceled init remained blocked in password input until pipe closed")
	}
}
