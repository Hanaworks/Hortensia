package service_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"docker-manager/internal/manager/httpapi"
	"docker-manager/internal/manager/service"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type unusedBackend struct{ httpapi.Backend }

func TestEarlyRejectionBodyDeadline(t *testing.T) {
	for _, path := range []string{"/api/v1/missing", "/api/v1/sessions?unexpected=1", "/api/v1/nodes"} {
		t.Run(path, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(nil)
			origin := "https://" + srv.Listener.Addr().String()
			h, err := httpapi.New(httpapi.Config{Store: &unusedBackend{}, ExternalOrigin: origin})
			if err != nil {
				t.Fatal(err)
			}
			srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
				defer cancel()
				h.ServeHTTP(w, r.WithContext(ctx))
			})
			srv.StartTLS()
			defer srv.Close()
			conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), srv.Client().Transport.(*http.Transport).TLSClientConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(700 * time.Millisecond))
			io.WriteString(conn, "POST "+path+" HTTP/1.1\r\nHost: "+srv.Listener.Addr().String()+"\r\nContent-Type: application/json\r\nContent-Length: 100\r\nOrigin: "+origin+"\r\n\r\n{")
			start := time.Now()
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("early rejection did not complete within 7x request deadline: elapsed=%s err=%v", time.Since(start), err)
			}
			defer resp.Body.Close()
			want := 400
			if path == "/api/v1/missing" {
				want = 404
			}
			if resp.StatusCode != want || !resp.Close {
				t.Fatalf("incorrect early rejection: status=%d close=%v", resp.StatusCode, resp.Close)
			}
			if time.Since(start) > 500*time.Millisecond {
				t.Fatal("late response")
			}
		})
	}
}

func runtimeConfig(t *testing.T) (service.Config, *tls.Config) {
	t.Helper()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	c := service.Config{Listen: fixture.Listener.Addr().String(), ExternalOrigin: fixture.URL, LogLevel: "info"}
	cert := fixture.TLS.Certificates[0]
	clientTLS := fixture.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	fixture.Close()
	dir := t.TempDir()
	c.TLSCertFile = filepath.Join(dir, "cert.pem")
	c.TLSKeyFile = filepath.Join(dir, "tls.key")
	c.DatabaseFile = filepath.Join(dir, "data", "manager.db")
	c.KeysDir = filepath.Join(dir, "keys")
	c.WebDir = filepath.Join(dir, "web")
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(c.TLSCertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(c.TLSKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(c.WebDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.WebDir, "index.html"), []byte("<!doctype html><title>probe</title>"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Initialize(context.Background(), c, "admin", "example password"); err != nil {
		t.Fatal(err)
	}
	return c, clientTLS
}

func TestStaticBodyDoesNotBlockResponse(t *testing.T) {
	c, clientTLS := runtimeConfig(t)
	runtime, err := service.Start(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		runtime.Close(ctx)
	}()
	for _, tc := range []struct{ method, path string }{{"GET", "/"}, {"POST", "/missing"}} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			conn, err := tls.Dial("tcp", c.Listen, clientTLS)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(700 * time.Millisecond))
			io.WriteString(conn, tc.method+" "+tc.path+" HTTP/1.1\r\nHost: "+c.Listen+"\r\nContent-Length: 100\r\n\r\n{")
			start := time.Now()
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("static request blocked after headers: elapsed=%s err=%v", time.Since(start), err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 400 || !resp.Close {
				t.Fatalf("static body must be rejected and connection closed: %d %v", resp.StatusCode, resp.Close)
			}
		})
	}
}

func TestConfigRejectsNonCanonicalPorts(t *testing.T) {
	dir := t.TempDir()
	for _, origin := range []string{"https://localhost:0443", "https://localhost:08443", "https://[0:0:0:0:0:0:0:1]:8443", "https://[::ffff:127.0.0.1]:8443", "https://127.1:8443", "https://0x7f000001:8443", "https://127.000.000.001:8443"} {
		t.Run(origin, func(t *testing.T) {
			c := service.Config{Listen: "127.0.0.1:8443", ExternalOrigin: origin, TLSCertFile: filepath.Join(dir, "cert.pem"), TLSKeyFile: filepath.Join(dir, "key.pem"), DatabaseFile: filepath.Join(dir, "data", "manager.db"), KeysDir: filepath.Join(dir, "keys"), WebDir: filepath.Join(dir, "web"), LogLevel: "info"}
			raw, _ := json.Marshal(c)
			if _, err := service.ParseConfig(raw); err == nil {
				t.Fatalf("noncanonical origin accepted: %s", origin)
			}
		})
	}
}
