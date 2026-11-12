// Package protocol validates the bundled v1 wire contract and bounded decoded
// configuration data. Session, direction, authorization, filesystem containment,
// and durable execution-state checks belong to the consuming state machine.
//
// Schema expressions preserve ECMAScript semantics. The fixed Base64 pattern
// uses a linear matcher; other expressions use a bounded ECMAScript engine.
// Runtime safe-relative-path checks are separate from the schema expression,
// including its treatment of newlines, and do not imply filesystem containment.
package protocol

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"docker-manager/contracts"
	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	maxMessageBytes = 8 << 20
	maxFileBytes    = 1 << 20
	maxConfigBytes  = 4 << 20
	// Numeric resource budgets apply before the schema engine can hash values
	// for uniqueItems. All numeric fields in v1 are safe integers.
	maxNumberBytes          = 128
	maxNumberExponent       = 128
	maxSafeInteger    int64 = 9007199254740991
	schemaID                = "https://docker-manager.invalid/contracts/agent-v1.schema.json"
)

var (
	errJSON     = errors.New("invalid_json")
	errSchema   = errors.New("schema_invalid")
	errEncoding = errors.New("encoding_invalid")
	errPayload  = errors.New("payload_invalid")
	errBudget   = errors.New("budget_exceeded")
	errNumber   = errors.New("number_invalid")
)

// Envelope is the validated wire envelope. Body retains the original JSON bytes.
type Envelope struct {
	Protocol  int             `json:"protocol"`
	Type      string          `json:"type"`
	MessageID string          `json:"message_id"`
	SessionID *string         `json:"session_id"`
	ReplyTo   *string         `json:"reply_to"`
	Body      json.RawMessage `json:"body"`
}

// Validator is immutable after construction and can be shared by connections.
type Validator struct {
	message *jsonschema.Schema
	payload *jsonschema.Schema
	config  *jsonschema.Schema
}

type offlineLoader struct{}

func (offlineLoader) Load(string) (any, error) {
	return nil, errors.New("external_schema_disabled")
}

// NewValidator compiles only the bundled schema and the library's embedded
// metaschemas. No external schema URL or filesystem reference is loaded.
func NewValidator() (*Validator, error) {
	doc, err := strictJSON(contracts.AgentV1Schema())
	if err != nil {
		return nil, errors.New("bundled_schema_invalid")
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(offlineLoader{})
	c.UseRegexpEngine(compilePattern)
	c.AssertFormat()
	c.RegisterFormat(&jsonschema.Format{Name: "date-time", Validate: func(value any) error {
		s, ok := value.(string)
		if !ok {
			return nil
		}
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			return errors.New("timestamp_invalid")
		}
		return nil
	}})
	if err := c.AddResource(schemaID, doc); err != nil {
		return nil, errors.New("bundled_schema_invalid")
	}
	message, err := c.Compile(schemaID)
	if err != nil {
		return nil, errors.New("bundled_schema_compile_failed")
	}
	payload, err := c.Compile(schemaID + "#/$defs/FrozenPayload")
	if err != nil {
		return nil, errors.New("bundled_payload_compile_failed")
	}
	config, err := c.Compile(schemaID + "#/$defs/Config")
	if err != nil {
		return nil, errors.New("bundled_config_compile_failed")
	}
	return &Validator{message: message, payload: payload, config: config}, nil
}

func safeRelativePath(s string) bool {
	if strings.ContainsAny(s, "\\:\x00") {
		return false
	}
	for _, segment := range strings.Split(s, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

type schemaPattern struct{ re *regexp2.Regexp }

const base64SchemaExpression = `^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`

type base64Pattern struct{}

func (base64Pattern) String() string { return base64SchemaExpression }

// This recognizes only the fixed Schema expression, in O(n) time and O(1)
// extra space. In the existing ECMAScript engine '$' requires absolute end,
// including rejection of a terminal LF (unlike its default .NET mode). Do
// not enforce canonical pad bits here: strict decoding and re-encoding
// perform that separate check after Schema validation.
func (base64Pattern) MatchString(s string) bool {
	if len(s)%4 != 0 {
		return false
	}
	end := len(s)
	if end > 0 && s[end-1] == '=' {
		end--
		if end > 0 && s[end-1] == '=' {
			end--
		}
	}
	for i := 0; i < end; i++ {
		b := s[i]
		if !(b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '+' || b == '/') {
			return false
		}
	}
	return true
}

func (p schemaPattern) String() string { return p.re.String() }

func (p schemaPattern) MatchString(s string) bool {
	matched, err := p.re.MatchString(s)
	return err == nil && matched
}

func compilePattern(pattern string) (jsonschema.Regexp, error) {
	if pattern == base64SchemaExpression {
		return base64Pattern{}, nil
	}
	re, err := regexp2.Compile(pattern, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	re.MatchTimeout = 100 * time.Millisecond
	return schemaPattern{re}, nil
}

// Validate checks a complete reassembled WebSocket text message. Errors contain
// only fixed diagnostic codes, never schema-engine details or untrusted values.
func (v *Validator) Validate(raw []byte) (Envelope, error) {
	if len(raw) > maxMessageBytes {
		return Envelope{}, errBudget
	}
	doc, err := strictJSON(raw)
	if err != nil {
		return Envelope{}, err
	}
	if err := v.message.Validate(doc); err != nil {
		return Envelope{}, errSchema
	}
	obj := doc.(map[string]any)
	body := obj["body"].(map[string]any)
	if err := checkEncodedValues(body); err != nil {
		return Envelope{}, err
	}
	if obj["type"] == "prepare" {
		if err := v.validatePrepare(body); err != nil {
			return Envelope{}, err
		}
	}
	// Schema accepts mathematically integral JSON numbers such as 1.0. Populate
	// the known protocol value separately instead of rejecting those encodings.
	var fields struct {
		Type      string          `json:"type"`
		MessageID string          `json:"message_id"`
		SessionID *string         `json:"session_id"`
		ReplyTo   *string         `json:"reply_to"`
		Body      json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Envelope{}, errJSON
	}
	return Envelope{1, fields.Type, fields.MessageID, fields.SessionID, fields.ReplyTo, fields.Body}, nil
}

func strictJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) || !validJSONUnicodeEscapes(raw) {
		return nil, errJSON
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, err := readJSONValue(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errJSON
	}
	return value, nil
}

func readJSONValue(d *json.Decoder, depth int) (any, error) {
	// Match encoding/json's nesting ceiling while bounding this recursive walk.
	if depth > 10000 {
		return nil, errJSON
	}
	token, err := d.Token()
	if err != nil {
		return nil, errJSON
	}
	if number, ok := token.(json.Number); ok {
		return boundedInteger(number)
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		obj := make(map[string]any)
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return nil, errJSON
			}
			key, ok := token.(string)
			if !ok {
				return nil, errJSON
			}
			if _, exists := obj[key]; exists {
				return nil, errJSON
			}
			value, err := readJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			obj[key] = value
		}
		if end, err := d.Token(); err != nil || end != json.Delim('}') {
			return nil, errJSON
		}
		return obj, nil
	case '[':
		arr := make([]any, 0)
		for d.More() {
			value, err := readJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, value)
		}
		if end, err := d.Token(); err != nil || end != json.Delim(']') {
			return nil, errJSON
		}
		return arr, nil
	default:
		return nil, errJSON
	}
}

// boundedInteger checks token resources before any arbitrary-precision work,
// then exactly normalizes integer encodings. Neither tiny nonzero fractions nor
// fractions beyond float64 precision may round into an accepted integer.
func boundedInteger(number json.Number) (json.Number, error) {
	token := number.String()
	if len(token) > maxNumberBytes {
		return "", errBudget
	}
	if index := strings.IndexAny(token, "eE"); index >= 0 {
		exponent, err := strconv.ParseInt(token[index+1:], 10, 16)
		if err != nil || exponent < -maxNumberExponent || exponent > maxNumberExponent {
			return "", errBudget
		}
	}
	value, ok := new(big.Rat).SetString(token)
	if !ok || !value.IsInt() || !value.Num().IsInt64() {
		return "", errNumber
	}
	integer := value.Num().Int64()
	if integer < -maxSafeInteger || integer > maxSafeInteger {
		return "", errNumber
	}
	return json.Number(strconv.FormatInt(integer, 10)), nil
}

func decodeBase64(value string) ([]byte, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != value {
		return nil, errEncoding
	}
	return raw, nil
}

// This walks only the schema-validated envelope body, which has fixed property
// names. The user-controlled interpolation environment is inside payload_b64
// and is deliberately not interpreted as protocol fields.
func checkEncodedValues(value any) error {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if s, ok := child.(string); ok {
				switch key {
				case "input_digest":
					raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
					if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != s {
						return errEncoding
					}
				case "payload_b64", "output_b64":
					raw, err := decodeBase64(s)
					if err != nil {
						return err
					}
					if key == "output_b64" && (len(raw) > maxFileBytes || !utf8.Valid(raw)) {
						return errEncoding
					}
				}
			}
			if err := checkEncodedValues(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := checkEncodedValues(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *Validator) validatePrepare(body map[string]any) error {
	raw, err := decodeBase64(body["payload_b64"].(string))
	if err != nil {
		return err
	}
	doc, err := strictJSON(raw)
	if err != nil {
		return err
	}
	if err := v.payload.Validate(doc); err != nil {
		return errPayload
	}
	payload := doc.(map[string]any)
	for _, key := range []string{"node_id", "stack_id", "version_id"} {
		if payload[key] != body[key] {
			return errPayload
		}
	}
	config := payload["config"].(map[string]any)
	return validateConfigFields(config)
}

// Both callers establish Config's shape with the shared Schema before this
// helper performs cross-field, byte-budget and canonical-encoding checks.
func validateConfigFields(config map[string]any) error {
	paths := make(map[string]struct{})
	total := 0
	for _, value := range config["files"].([]any) {
		file := value.(map[string]any)
		path := file["path"].(string)
		if !safeRelativePath(path) {
			return errPayload
		}
		if len(path) > 240 {
			return errBudget
		}
		if _, exists := paths[path]; exists {
			return errPayload
		}
		paths[path] = struct{}{}
		content, err := decodeBase64(file["content_b64"].(string))
		if err != nil {
			return err
		}
		if !utf8.Valid(content) {
			return errEncoding
		}
		total += len(content)
		if len(content) > maxFileBytes || total > maxConfigBytes {
			return errBudget
		}
	}
	for _, key := range []string{"compose_files", "interpolation_env_files"} {
		for _, value := range config[key].([]any) {
			path := value.(string)
			if !safeRelativePath(path) {
				return errPayload
			}
			if len(path) > 240 {
				return errBudget
			}
			if _, exists := paths[path]; !exists {
				return errPayload
			}
		}
	}
	return nil
}

// NewMessage creates an envelope; the caller validates the message and applies
// direction/session/state checks before sending it. Empty references are null.
func NewMessage(kind, session, reply string, body any) ([]byte, error) {
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("message_encode_failed")
	}
	var sessionID, replyTo *string
	if session != "" {
		sessionID = &session
	}
	if reply != "" {
		replyTo = &reply
	}
	raw, err := json.Marshal(Envelope{1, kind, id, sessionID, replyTo, bodyJSON})
	if err != nil {
		return nil, errors.New("message_encode_failed")
	}
	return raw, nil
}

// NewID returns a cryptographically random lowercase UUIDv4.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("message_id_failed")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
