package service

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func prepareTLS(t *testing.T, c Config, host string) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	secret := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(c.TLSCertFile, cert, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.TLSKeyFile, secret, 0600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(cert)
	return pool
}

func prepareWeb(t *testing.T, c Config) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(c.WebDir, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.WebDir, "index.html"), []byte("<!doctype html><title>Manager test</title>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.WebDir, "assets", "app-1234abcd.js"), []byte("'use strict';"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestWebStaticBoundary(t *testing.T) {
	c := runtimeConfig(t)
	prepareWeb(t, c)
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":"api"}`)
	})
	h, err := newWebHandler(c.WebDir, api)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path     string
		status   int
		contains string
	}{{"/", 200, "Manager test"}, {"/assets/app-1234abcd.js", 200, "use strict"}, {"/assets/", 404, ""}, {"/missing", 404, ""}, {"/../identity.json", 404, ""}, {"/%2e%2e/identity.json", 404, ""}, {"/api/v1/missing", 404, `"api"`}, {"/agent/v1/missing", 404, `"api"`}} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || !bytes.Contains(w.Body.Bytes(), []byte(tc.contains)) {
				t.Fatalf("response %d %s", w.Code, w.Body.String())
			}
			if tc.status == 200 && w.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("missing static CSP")
			}
		})
	}
}

func TestManagerStartsTLSAndReleasesOnShutdown(t *testing.T) {
	c := runtimeConfig(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.Listen = ln.Addr().String()
	ln.Close()
	_, port, _ := net.SplitHostPort(c.Listen)
	c.ExternalOrigin = "https://localhost:" + port
	pool := prepareTLS(t, c, "localhost")
	prepareWeb(t, c)
	if _, err := Initialize(context.Background(), c, "admin", "example password"); err != nil {
		t.Fatal(err)
	}
	run, err := Start(c)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close(context.Background())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	req, _ := http.NewRequest("POST", c.ExternalOrigin+"/api/v1/sessions", bytes.NewBufferString(`{"username":"admin","password":"example password"}`))
	req.Header.Set("Origin", c.ExternalOrigin)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(resp.Cookies()) != 1 || !resp.Cookies()[0].Secure {
		t.Fatalf("login %d %s", resp.StatusCode, body)
	}
	if other, err := Start(c); err == nil {
		other.Close(context.Background())
		t.Fatal("second manager started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := run.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Start(c)
	if err != nil {
		t.Fatal("restart failed", err)
	}
	defer reopened.Close(context.Background())
}

func TestBadTLSCannotOpenDatabase(t *testing.T) {
	c := runtimeConfig(t)
	prepareTLS(t, c, "wrong.example")
	prepareWeb(t, c)
	if _, err := Initialize(context.Background(), c, "admin", "example password"); err != nil {
		t.Fatal(err)
	}
	if run, err := Start(c); err == nil {
		run.Close(context.Background())
		t.Fatal("wrong certificate identity accepted")
	}
}

func TestRuntimeRejectsDamagedIdentityWithoutRepair(t *testing.T) {
	for _, kind := range []string{"missing_identity", "short_key", "wrong_key", "future_manifest"} {
		t.Run(kind, func(t *testing.T) {
			c := runtimeConfig(t)
			prepareTLS(t, c, "localhost")
			prepareWeb(t, c)
			if _, err := Initialize(context.Background(), c, "admin", "example password"); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(c.KeysDir, "encryption.key")
			if kind == "missing_identity" || kind == "future_manifest" {
				file = filepath.Join(filepath.Dir(c.DatabaseFile), "identity.json")
			}
			original, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var damaged []byte
			switch kind {
			case "missing_identity":
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			case "short_key":
				damaged = []byte("short")
			case "wrong_key":
				damaged = bytes.Repeat([]byte{7}, 32)
			case "future_manifest":
				damaged = bytes.Replace(original, []byte(`"schema_version":5`), []byte(`"schema_version":99`), 1)
			}
			if kind != "missing_identity" {
				if err := os.WriteFile(file, damaged, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if running, err := Start(c); err == nil {
				running.Close(context.Background())
				t.Fatal("damaged material accepted")
			}
			after, err := os.ReadFile(file)
			if kind == "missing_identity" {
				if !os.IsNotExist(err) {
					t.Fatal("missing identity was recreated")
				}
			} else if err != nil || !bytes.Equal(after, damaged) {
				t.Fatal("failed Start repaired or replaced evidence")
			}
			if err := os.WriteFile(file, original, 0600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeListenFailureAndForcedCloseReleaseOwnership(t *testing.T) {
	c := runtimeConfig(t)
	pool := prepareTLS(t, c, "localhost")
	prepareWeb(t, c)
	if _, err := Initialize(context.Background(), c, "admin", "example password"); err != nil {
		t.Fatal(err)
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.Listen = occupied.Addr().String()
	if run, err := Start(c); err == nil {
		run.Close(context.Background())
		t.Fatal("occupied listener accepted")
	}
	occupied.Close()
	run, err := Start(c)
	if err != nil {
		t.Fatal("listen failure retained lock", err)
	}
	conn, err := tls.Dial("tcp", c.Listen, &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		run.Close(context.Background())
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "POST /api/v1/sessions HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	closing, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := run.Close(closing); err != nil {
		t.Fatal(err)
	}
	reopened, err := Start(c)
	if err != nil {
		t.Fatal("forced close retained Store or lock", err)
	}
	defer reopened.Close(context.Background())
}
