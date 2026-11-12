// Package service owns the Manager process lifecycle and local identity files.
package service

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"docker-manager/internal/protocol"
)

var (
	ErrConfig      = errors.New("manager configuration invalid")
	ErrFiles       = errors.New("manager private files invalid")
	ErrLocked      = errors.New("manager already running")
	ErrInitialized = errors.New("manager initialization materials already exist")
	ErrTLS         = errors.New("manager TLS configuration invalid")
)

const maxConfigFile = 64 << 10

type Config struct {
	Listen         string `json:"listen"`
	ExternalOrigin string `json:"external_origin"`
	TLSCertFile    string `json:"tls_cert_file"`
	TLSKeyFile     string `json:"tls_key_file"`
	DatabaseFile   string `json:"database_file"`
	KeysDir        string `json:"keys_dir"`
	LogLevel       string `json:"log_level"`
	WebDir         string `json:"web_dir"`
}

func LoadConfig(path string) (Config, error) {
	if !filepath.IsAbs(path) {
		return Config{}, ErrConfig
	}
	if rejectLinks(path) != nil {
		return Config{}, ErrConfig
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxConfigFile {
		return Config{}, ErrConfig
	}
	f, err := os.Open(path)
	if err != nil {
		return Config{}, ErrConfig
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxConfigFile+1))
	if err != nil {
		return Config{}, ErrConfig
	}
	return ParseConfig(raw)
}

func ParseConfig(raw []byte) (Config, error) {
	if len(raw) > maxConfigFile {
		return Config{}, ErrConfig
	}
	value, err := protocol.StrictJSON(raw)
	if err != nil {
		return Config{}, ErrConfig
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return Config{}, ErrConfig
	}
	for name, value := range obj {
		switch name {
		case "listen", "external_origin", "tls_cert_file", "tls_key_file", "database_file", "keys_dir", "log_level", "web_dir":
		default:
			return Config{}, ErrConfig
		}
		if _, ok := value.(string); !ok {
			return Config{}, ErrConfig
		}
	}
	c := Config{Listen: "127.0.0.1:8443", DatabaseFile: "/var/lib/docker-manager/manager.db", KeysDir: "/etc/docker-manager/keys", LogLevel: "info", WebDir: "/usr/share/docker-manager/web"}
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, ErrConfig
	}
	if _, err := originHost(c.ExternalOrigin); err != nil {
		return Config{}, err
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil || strings.ContainsAny(host, "/\\\x00\r\n\t ") || !validPort(port) {
		return Config{}, ErrConfig
	}
	for _, path := range []string{c.TLSCertFile, c.TLSKeyFile, c.DatabaseFile, c.KeysDir, c.WebDir} {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return Config{}, ErrConfig
		}
	}
	if c.LogLevel != "info" && c.LogLevel != "warn" && c.LogLevel != "error" {
		return Config{}, ErrConfig
	}
	if filepath.Clean(c.TLSCertFile) == filepath.Clean(c.TLSKeyFile) || filepath.Clean(c.DatabaseFile) == filepath.Clean(c.KeysDir) {
		return Config{}, ErrConfig
	}
	return c, nil
}

func validPort(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(value)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == value
}

func originHost(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.String() != value || strings.ToLower(u.Host) != u.Host {
		return "", ErrConfig
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, "/\\\x00\r\n\t ") {
		return "", ErrConfig
	}
	for _, c := range host {
		if c > 127 {
			return "", ErrConfig
		}
	}
	if strings.Contains(host, ":") {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Is6() || ip.Zone() != "" || canonicalIPv6(ip) != host {
			return "", ErrConfig
		}
	} else if ip, err := netip.ParseAddr(host); err == nil {
		if !ip.Is4() || ip.String() != host {
			return "", ErrConfig
		}
	} else {
		// Browsers parse numeric final labels as legacy IPv4 forms. Refuse
		// those rather than saving an origin different from URL.origin.
		labels := strings.Split(strings.TrimSuffix(host, "."), ".")
		last := labels[len(labels)-1]
		if browserNumericLabel(last) {
			return "", ErrConfig
		}
		if len(host) > 253 {
			return "", ErrConfig
		}
		for _, label := range labels {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", ErrConfig
			}
			for _, b := range label {
				if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-') {
					return "", ErrConfig
				}
			}
		}
	}
	if port := u.Port(); port != "" && (!validPort(port) || port == "443") {
		return "", ErrConfig
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", ErrConfig
	}
	return host, nil
}

func browserNumericLabel(label string) bool {
	digits := label != ""
	for _, c := range label {
		if c < '0' || c > '9' {
			digits = false
			break
		}
	}
	if digits {
		return true
	}
	if !strings.HasPrefix(label, "0x") {
		return false
	}
	for _, c := range label[2:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// WHATWG URL serialization uses hexadecimal IPv6 tails even for IPv4-mapped
// addresses. netip.String alone does not give the browser's origin spelling.
func canonicalIPv6(ip netip.Addr) string {
	raw := ip.As16()
	words := make([]uint16, 8)
	for i := range words {
		words[i] = uint16(raw[2*i])<<8 | uint16(raw[2*i+1])
	}
	bestStart, bestLen := -1, 1
	for i := 0; i < 8; {
		if words[i] != 0 {
			i++
			continue
		}
		j := i
		for j < 8 && words[j] == 0 {
			j++
		}
		if j-i > bestLen {
			bestStart, bestLen = i, j-i
		}
		i = j
	}
	text := make([]string, 8)
	for i, w := range words {
		text[i] = strconv.FormatUint(uint64(w), 16)
	}
	if bestStart < 0 {
		return strings.Join(text, ":")
	}
	return strings.Join(text[:bestStart], ":") + "::" + strings.Join(text[bestStart+bestLen:], ":")
}
