package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFixture(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	return map[string]string{"external_origin": "https://localhost:8443", "tls_cert_file": filepath.Join(dir, "cert.pem"), "tls_key_file": filepath.Join(dir, "key.pem"), "database_file": filepath.Join(dir, "data", "manager.db"), "keys_dir": filepath.Join(dir, "keys"), "web_dir": filepath.Join(dir, "web")}
}

func TestConfigDefaultsAndExactValues(t *testing.T) {
	m := configFixture(t)
	raw, _ := json.Marshal(m)
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8443" || c.LogLevel != "info" || c.ExternalOrigin != m["external_origin"] || c.DatabaseFile != m["database_file"] {
		t.Fatalf("configuration changed: %+v", c)
	}
	m["listen"] = "[::1]:9443"
	m["log_level"] = "warn"
	raw, _ = json.Marshal(m)
	c, err = ParseConfig(raw)
	if err != nil || c.Listen != "[::1]:9443" || c.LogLevel != "warn" {
		t.Fatalf("explicit values: %+v %v", c, err)
	}
}

func TestConfigRejectsMalformedAndUnsafeInputs(t *testing.T) {
	base, _ := json.Marshal(configFixture(t))
	bad := [][]byte{[]byte(`null`), []byte(`[]`), []byte(`{"external_origin":"https://localhost","external_origin":"https://localhost"}`), append(append([]byte{}, base...), []byte(` {}`)...), []byte(`{"external_origin":"\ud800"}`), []byte(`{"external_origin":"` + string([]byte{255}) + `"}`), []byte(strings.Repeat(" ", 64<<10) + "{}")}
	for _, raw := range bad {
		if _, err := ParseConfig(raw); err == nil {
			t.Fatal("malformed configuration accepted")
		}
	}
	for field, values := range map[string][]any{
		"external_origin": {nil, 1, "http://localhost", "https://user@localhost", "https://localhost/", "https://localhost?x=1", "https://localhost#x", "https://localhost:443", "https://LOCALHOST:8443", "https://localhost:0"},
		"listen":          {nil, 1, "localhost", "localhost:0", "localhost:65536", "http://localhost:8443"},
		"database_file":   {nil, "relative.db"}, "tls_key_file": {"key.pem"}, "keys_dir": {"keys"}, "log_level": {"debug"}, "unknown": {"x"},
	} {
		for _, value := range values {
			t.Run(field, func(t *testing.T) {
				data := map[string]any{}
				for k, v := range configFixture(t) {
					data[k] = v
				}
				data[field] = value
				raw, _ := json.Marshal(data)
				if _, err := ParseConfig(raw); err == nil {
					t.Fatalf("accepted field %s", field)
				}
			})
		}
	}
}

func TestLoadConfigHasNoCreationSideEffects(t *testing.T) {
	m := configFixture(t)
	raw, _ := json.Marshal(m)
	path := filepath.Join(t.TempDir(), "manager.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{m["database_file"], m["keys_dir"]} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("configuration load created %s", p)
		}
	}
	if _, err := LoadConfig("relative.json"); err == nil {
		t.Fatal("relative configuration accepted")
	}
}

func TestCanonicalBrowserOriginsRemainAccepted(t *testing.T) {
	for _, origin := range []string{"https://localhost", "https://node.example:8443", "https://127.0.0.1:8443", "https://[::1]:8443", "https://[2001:db8::1]", "https://[::ffff:7f00:1]:8443", "https://0xyz"} {
		t.Run(origin, func(t *testing.T) {
			m := configFixture(t)
			m["external_origin"] = origin
			raw, _ := json.Marshal(m)
			c, err := ParseConfig(raw)
			if err != nil || c.ExternalOrigin != origin {
				t.Fatalf("canonical origin refused: %v", err)
			}
		})
	}
}
