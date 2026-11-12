// Package service owns the Agent process and consumes only formal runtime APIs.
package service

import (
	"docker-manager/internal/localstate"
	"docker-manager/internal/protocol"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var ErrConfig = errors.New("agent configuration invalid")
var ErrIdentity = errors.New("agent identity invalid")
var ErrProtocol = errors.New("agent protocol rejected")
var ErrTransport = errors.New("agent connection unavailable")
var ErrUnauthorized = errors.New("agent credentials rejected")
var ErrRuntime = errors.New("agent runtime unavailable")

type Config struct {
	ManagerOrigin           string `json:"manager_origin"`
	CAFile                  string `json:"ca_file,omitempty"`
	StateDir                string `json:"state_dir"`
	IdentityFile            string `json:"identity_file"`
	KeysDir                 string `json:"keys_dir"`
	DockerCLI               string `json:"docker_cli"`
	DockerSocket            string `json:"docker_socket"`
	ExecutionTimeoutSeconds int    `json:"execution_timeout_seconds"`
	LogLevel                string `json:"log_level"`
}

func LoadConfig(path string) (Config, error) {
	if !filepath.IsAbs(path) || localstate.RejectLinks(path) != nil {
		return Config{}, ErrConfig
	}
	f, e := os.Open(path)
	if e != nil {
		return Config{}, ErrConfig
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Size() > 64<<10 {
		return Config{}, ErrConfig
	}
	b, e := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if e != nil {
		return Config{}, ErrConfig
	}
	return ParseConfig(b)
}
func ParseConfig(b []byte) (Config, error) {
	if len(b) > 64<<10 {
		return Config{}, ErrConfig
	}
	v, e := protocol.StrictJSON(b)
	if e != nil {
		return Config{}, ErrConfig
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return Config{}, ErrConfig
	}
	for k, v := range obj {
		if k == "execution_timeout_seconds" {
			switch v.(type) {
			case json.Number, float64:
			default:
				return Config{}, ErrConfig
			}
			continue
		}
		switch k {
		case "manager_origin", "ca_file", "state_dir", "identity_file", "keys_dir", "docker_cli", "docker_socket", "log_level":
		default:
			return Config{}, ErrConfig
		}
		if _, ok := v.(string); !ok {
			return Config{}, ErrConfig
		}
	}
	c := Config{StateDir: "/var/lib/docker-manager-agent", IdentityFile: "/etc/docker-manager-agent/identity.json", KeysDir: "/etc/docker-manager-agent/keys", DockerSocket: "unix:///var/run/docker.sock", ExecutionTimeoutSeconds: 1800, LogLevel: "info"}
	if json.Unmarshal(b, &c) != nil {
		return Config{}, ErrConfig
	}
	u, e := url.Parse(c.ManagerOrigin)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.String() != c.ManagerOrigin || strings.ToLower(u.Host) != u.Host {
		return Config{}, ErrConfig
	}
	if strings.HasSuffix(u.Host, ":") || strings.ContainsAny(u.Hostname(), "%\\ \x00\r\n\t") {
		return Config{}, ErrConfig
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return Config{}, ErrConfig
		}
	}
	socket, err := url.Parse(c.DockerSocket)
	if err != nil || socket.Scheme != "unix" || socket.Host != "" || socket.RawPath != "" || socket.Path == "" || !strings.HasPrefix(socket.Path, "/") || socket.User != nil || socket.RawQuery != "" || socket.ForceQuery || socket.Fragment != "" {
		return Config{}, ErrConfig
	}
	for _, p := range []string{c.StateDir, c.IdentityFile, c.KeysDir, c.DockerCLI} {
		if !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
			return Config{}, ErrConfig
		}
	}
	if c.CAFile != "" && (!filepath.IsAbs(c.CAFile) || strings.ContainsRune(c.CAFile, 0)) {
		return Config{}, ErrConfig
	}
	if _, provided := obj["ca_file"]; provided && c.CAFile == "" {
		return Config{}, ErrConfig
	}
	if !strings.HasPrefix(c.DockerSocket, "unix:///") || strings.ContainsAny(c.DockerSocket, "\x00\r\n?#") || strings.TrimPrefix(c.DockerSocket, "unix://") == "/" || c.ExecutionTimeoutSeconds < 1 || c.ExecutionTimeoutSeconds > 86400 {
		return Config{}, ErrConfig
	}
	if c.LogLevel != "info" && c.LogLevel != "warn" && c.LogLevel != "error" {
		return Config{}, ErrConfig
	}
	paths := map[string]bool{}
	for _, p := range []string{c.IdentityFile, filepath.Join(c.KeysDir, "ledger.key"), filepath.Join(c.KeysDir, "journal.key"), filepath.Join(c.StateDir, "agent.db"), filepath.Join(c.StateDir, "agent.db-wal"), filepath.Join(c.StateDir, "agent.db-shm"), filepath.Join(c.StateDir, "agent.db-journal"), filepath.Join(c.StateDir, ".agent.lock")} {
		p = filepath.Clean(p)
		if runtime.GOOS == "windows" {
			p = strings.ToLower(p)
		}
		if paths[p] {
			return Config{}, ErrConfig
		}
		paths[p] = true
	}
	return c, nil
}
func validateConfig(c Config) error {
	b, e := json.Marshal(c)
	if e != nil {
		return ErrConfig
	}
	_, e = ParseConfig(b)
	return e
}
