package backup

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const groupJSONLimit int64 = 256 << 10
const groupIdentityLimit int64 = 64 << 10

type fileDigest struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type byteDigest struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type secretFileDigest struct {
	Role   MaterialRole `json:"role"`
	KeyID  *string      `json:"key_id"`
	Path   string       `json:"path"`
	Bytes  int64        `json:"bytes"`
	SHA256 string       `json:"sha256"`
}
type inspectionDigest struct {
	SemanticFormat      string     `json:"semantic_format"`
	SemanticFingerprint string     `json:"semantic_fingerprint"`
	TableRows           []RowCount `json:"table_rows"`
	SealedRows          []RowCount `json:"sealed_rows"`
}
type dataManifest struct {
	Format                     string             `json:"format"`
	BackupID                   string             `json:"backup_id"`
	Component                  Component          `json:"component"`
	SourceLayout               string             `json:"source_layout"`
	Generation                 *string            `json:"generation"`
	Identity                   IdentityRef        `json:"identity"`
	SchemaVersion              int                `json:"schema_version"`
	ProtocolVersion            int                `json:"protocol_version"`
	Creator                    BuildInfo          `json:"creator"`
	SQLiteVersion              string             `json:"sqlite_version"`
	StartedAt                  string             `json:"started_at"`
	CompletedAt                string             `json:"completed_at"`
	Database                   fileDigest         `json:"database"`
	Files                      []fileDigest       `json:"files"`
	IdentitySHA256             string             `json:"identity_sha256"`
	Keys                       []KeyRef           `json:"keys"`
	ReferencedEncryptionKeyIDs []string           `json:"referenced_encryption_key_ids"`
	Inspection                 inspectionDigest   `json:"inspection"`
	SecretManifest             byteDigest         `json:"secret_manifest"`
	ExternalMaterials          []ExternalMaterial `json:"external_materials"`
}
type secretManifest struct {
	Format         string             `json:"format"`
	BackupID       string             `json:"backup_id"`
	Component      Component          `json:"component"`
	SourceLayout   string             `json:"source_layout"`
	Generation     *string            `json:"generation"`
	Identity       IdentityRef        `json:"identity"`
	DatabaseSHA256 string             `json:"database_sha256"`
	DatabaseBytes  int64              `json:"database_bytes"`
	IdentitySHA256 string             `json:"identity_sha256"`
	Files          []secretFileDigest `json:"files"`
}
type agentBackupConfig struct {
	Format                  string    `json:"format"`
	Component               Component `json:"component"`
	SuppliedConfig          bool      `json:"supplied_config"`
	ManagerOrigin           string    `json:"manager_origin"`
	DockerSocket            string    `json:"docker_socket"`
	ExecutionTimeoutSeconds int       `json:"execution_timeout_seconds"`
	LogLevel                string    `json:"log_level"`
	TrustRootsMode          string    `json:"trust_roots_mode"`
}
type managerBackupConfig struct {
	Format         string    `json:"format"`
	Component      Component `json:"component"`
	SuppliedConfig bool      `json:"supplied_config"`
	Listen         string    `json:"listen"`
	ExternalOrigin string    `json:"external_origin"`
	LogLevel       string    `json:"log_level"`
}

func strictGroupJSON(raw []byte, target any) error {
	if len(raw) == 0 || int64(len(raw)) > groupJSONLimit {
		return ErrVerification
	}
	value, err := groupJSONValue(raw)
	if err != nil || !groupShape(value, reflect.TypeOf(target).Elem()) {
		return ErrVerification
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return ErrVerification
	}
	return nil
}

// The protocol decoder intentionally limits numbers to JavaScript's safe
// integers. Backup metadata instead declares uint64 counts, so keep numbers
// exact while enforcing the same Unicode and decoded-key uniqueness rules.
func groupJSONValue(raw []byte) (any, error) {
	if !utf8.Valid(raw) || !groupUnicodeEscapes(raw) {
		return nil, ErrVerification
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, e := groupJSONToken(d, 0)
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, ErrVerification
	}
	return value, nil
}
func groupJSONToken(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, ErrVerification
	}
	token, e := d.Token()
	if e != nil {
		return nil, ErrVerification
	}
	opening, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch opening {
	case '{':
		object := map[string]any{}
		for d.More() {
			key, e := d.Token()
			name, ok := key.(string)
			if e != nil || !ok {
				return nil, ErrVerification
			}
			if _, duplicate := object[name]; duplicate {
				return nil, ErrVerification
			}
			value, e := groupJSONToken(d, depth+1)
			if e != nil {
				return nil, e
			}
			object[name] = value
		}
		if end, e := d.Token(); e != nil || end != json.Delim('}') {
			return nil, ErrVerification
		}
		return object, nil
	case '[':
		list := []any{}
		for d.More() {
			value, e := groupJSONToken(d, depth+1)
			if e != nil {
				return nil, e
			}
			list = append(list, value)
		}
		if end, e := d.Token(); e != nil || end != json.Delim(']') {
			return nil, ErrVerification
		}
		return list, nil
	default:
		return nil, ErrVerification
	}
}
func groupUnicodeEscapes(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if !inString {
			if raw[i] == '"' {
				inString = true
			}
			continue
		}
		if raw[i] == '"' {
			inString = false
			continue
		}
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		value, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if value >= 0xd800 && value <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		} else if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
	}
	return !inString
}

// Every declared field is required. Pointers alone permit explicit null;
// generation/key_id/creator pointer rules are then constrained by the caller.
func groupShape(value any, kind reflect.Type) bool {
	if kind.Kind() == reflect.Pointer {
		return value == nil || groupShape(value, kind.Elem())
	}
	if value == nil {
		return false
	}
	switch kind.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok || len(object) != kind.NumField() {
			return false
		}
		for i := 0; i < kind.NumField(); i++ {
			field := kind.Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			v, present := object[name]
			if !present || !groupShape(v, field.Type) {
				return false
			}
		}
	case reflect.Slice:
		list, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range list {
			if !groupShape(v, kind.Elem()) {
				return false
			}
		}
	}
	return true
}
func validateGroupConfig(c Component, raw []byte) error {
	validLog := func(s string) bool { return s == "info" || s == "warn" || s == "error" }
	switch c {
	case ComponentAgent:
		var v agentBackupConfig
		if strictGroupJSON(raw, &v) != nil || v.Format != "dm-backup-config-v1" || v.Component != c || !v.SuppliedConfig || v.ManagerOrigin == "" || !strings.HasPrefix(v.DockerSocket, "unix:///") || v.ExecutionTimeoutSeconds < 1 || v.ExecutionTimeoutSeconds > 86400 || !validLog(v.LogLevel) || (v.TrustRootsMode != "system" && v.TrustRootsMode != "custom") {
			return ErrInput
		}
	case ComponentManager:
		var v managerBackupConfig
		if strictGroupJSON(raw, &v) != nil || v.Format != "dm-backup-config-v1" || v.Component != c || !v.SuppliedConfig || v.Listen == "" || v.ExternalOrigin == "" || !validLog(v.LogLevel) {
			return ErrInput
		}
	default:
		return ErrInput
	}
	return nil
}

var groupUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func groupToken32(s string) bool {
	raw, e := base64.RawURLEncoding.Strict().DecodeString(s)
	return e == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == s
}
func groupRoles(c Component) []MaterialRole {
	switch c {
	case ComponentAgent:
		return []MaterialRole{RoleIdentity, RoleJournal, RoleLedger}
	case ComponentManager:
		return []MaterialRole{RoleDigest, RoleEncryption, RoleIdentity}
	default:
		return nil
	}
}
func groupMaterialPath(role MaterialRole) string {
	if role == RoleIdentity {
		return "identity.json"
	}
	return "keys/" + string(role) + ".key"
}
func normalizeReport(r DomainReport) (DomainReport, error) {
	if (r.Component != ComponentAgent && r.Component != ComponentManager) || r.SchemaVersion != 5 || r.ProtocolVersion != 1 || r.SQLiteVersion == "" || !groupToken32(r.SemanticFingerprint) || r.SemanticFormat != "dm-"+string(r.Component)+"-runtime-semantic-v1" {
		return DomainReport{}, ErrVerification
	}
	if r.Component == ComponentAgent {
		if r.Identity.ManagerID != "" || !groupUUID.MatchString(r.Identity.NodeID) || !groupUUID.MatchString(r.Identity.LedgerID) {
			return DomainReport{}, ErrVerification
		}
	} else if !groupUUID.MatchString(r.Identity.ManagerID) || r.Identity.NodeID != "" || r.Identity.LedgerID != "" {
		return DomainReport{}, ErrVerification
	}
	r.Keys = append([]KeyRef{}, r.Keys...)
	sort.Slice(r.Keys, func(i, j int) bool { return r.Keys[i].Role < r.Keys[j].Role })
	var roles []MaterialRole
	for _, role := range groupRoles(r.Component) {
		if role != RoleIdentity {
			roles = append(roles, role)
		}
	}
	if len(r.Keys) != 2 {
		return DomainReport{}, ErrVerification
	}
	for i, k := range r.Keys {
		if k.Role != roles[i] || !groupUUID.MatchString(k.KeyID) {
			return DomainReport{}, ErrVerification
		}
	}
	if r.Keys[0].KeyID == r.Keys[1].KeyID {
		return DomainReport{}, ErrVerification
	}
	normalizeCounts := func(input []RowCount) ([]RowCount, bool) {
		if len(input) == 0 {
			return nil, false
		}
		out := append([]RowCount{}, input...)
		sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
		for i, r := range out {
			if r.Table == "" || i > 0 && out[i-1].Table == r.Table {
				return nil, false
			}
		}
		return out, true
	}
	var ok bool
	r.TableRows, ok = normalizeCounts(r.TableRows)
	if !ok {
		return DomainReport{}, ErrVerification
	}
	r.SealedRows, ok = normalizeCounts(r.SealedRows)
	if !ok {
		return DomainReport{}, ErrVerification
	}
	for _, sealed := range r.SealedRows {
		found := false
		for _, table := range r.TableRows {
			if table.Table == sealed.Table && sealed.Rows <= table.Rows {
				found = true
				break
			}
		}
		if !found {
			return DomainReport{}, ErrVerification
		}
	}
	r.ReferencedEncryptionKeyIDs = append([]string{}, r.ReferencedEncryptionKeyIDs...)
	sort.Strings(r.ReferencedEncryptionKeyIDs)
	encrypted := false
	for _, row := range r.SealedRows {
		encrypted = encrypted || row.Rows > 0
	}
	wantRole := RoleJournal
	if r.Component == ComponentManager {
		wantRole = RoleEncryption
	}
	var encryptionID string
	for _, k := range r.Keys {
		if k.Role == wantRole {
			encryptionID = k.KeyID
		}
	}
	if encrypted {
		if len(r.ReferencedEncryptionKeyIDs) != 1 || r.ReferencedEncryptionKeyIDs[0] != encryptionID {
			return DomainReport{}, ErrVerification
		}
	} else if len(r.ReferencedEncryptionKeyIDs) != 0 {
		return DomainReport{}, ErrVerification
	}
	return r, nil
}
func groupExternal(c Component) []ExternalMaterial {
	result := []ExternalMaterial{{"docker_images", false, "excluded"}, {"docker_data_root", false, "excluded"}, {"application_volumes_and_binds", false, "excluded"}, {"external_databases", false, "excluded"}}
	if c == ComponentAgent {
		result = append(result, ExternalMaterial{"docker_compose_cli", true, "provision_supported_version"}, ExternalMaterial{"engine_endpoint", true, "revalidate_endpoint"}, ExternalMaterial{"tls_trust_roots", true, "provision_current"}, ExternalMaterial{"agent_workspaces", true, "preserve_and_reconcile"}, ExternalMaterial{"agent_operations", true, "preserve_and_reconcile"})
	} else if c == ComponentManager {
		result = append(result, ExternalMaterial{"tls_server_certificate", true, "provision_current"}, ExternalMaterial{"tls_server_private_key", true, "provision_current"}, ExternalMaterial{"web_assets", true, "matching_application_build"})
	} else {
		return nil
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Kind < result[j].Kind })
	return result
}
func normalizeExternal(c Component, input []ExternalMaterial) ([]ExternalMaterial, error) {
	out := append([]ExternalMaterial{}, input...)
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	if !reflect.DeepEqual(out, groupExternal(c)) {
		return nil, ErrInput
	}
	return out, nil
}
func groupJSON(v any) ([]byte, error) {
	raw, e := json.Marshal(v)
	if e != nil || int64(len(raw)+1) > groupJSONLimit {
		return nil, ErrVerification
	}
	return append(raw, '\n'), nil
}
func manifestReport(m dataManifest) DomainReport {
	return DomainReport{Component: m.Component, Identity: m.Identity, SchemaVersion: m.SchemaVersion, ProtocolVersion: m.ProtocolVersion, SQLiteVersion: m.SQLiteVersion, Keys: m.Keys, ReferencedEncryptionKeyIDs: m.ReferencedEncryptionKeyIDs, TableRows: m.Inspection.TableRows, SealedRows: m.Inspection.SealedRows, SemanticFormat: m.Inspection.SemanticFormat, SemanticFingerprint: m.Inspection.SemanticFingerprint}
}
func validateDataManifest(m dataManifest) error {
	if m.Format != "dm-backup-v1" || !groupUUID.MatchString(m.BackupID) || m.SourceLayout != "flat-v1" || m.Generation != nil || m.Creator.ApplicationVersion == "" || m.Creator.BuildRevision != nil && *m.Creator.BuildRevision == "" || m.Database.Path != "sqlite/state.db" || m.Database.Bytes < 1 || !groupToken32(m.Database.SHA256) || len(m.Files) != 1 || m.Files[0].Path != "config.json" || m.Files[0].Bytes < 1 || m.Files[0].Bytes > groupJSONLimit || !groupToken32(m.Files[0].SHA256) || !groupToken32(m.IdentitySHA256) || m.SecretManifest.Bytes < 1 || m.SecretManifest.Bytes > groupJSONLimit || !groupToken32(m.SecretManifest.SHA256) {
		return ErrVerification
	}
	for _, value := range []string{m.StartedAt, m.CompletedAt} {
		parsed, e := time.Parse(time.RFC3339Nano, value)
		if e != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != value {
			return ErrVerification
		}
	}
	report, e := normalizeReport(manifestReport(m))
	if e != nil || !reflect.DeepEqual(report, manifestReport(m)) {
		return ErrVerification
	}
	ext, e := normalizeExternal(m.Component, m.ExternalMaterials)
	if e != nil || !reflect.DeepEqual(ext, m.ExternalMaterials) {
		return ErrVerification
	}
	return nil
}
