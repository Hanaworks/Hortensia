package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

func fixtures(t *testing.T) []json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("../../contracts/examples/agent-v1.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(b, &values); err != nil {
		t.Fatal(err)
	}
	return values
}

func fixture(t *testing.T, kind string) map[string]any {
	t.Helper()
	for _, raw := range fixtures(t) {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if value["type"] == kind {
			return value
		}
	}
	t.Fatalf("fixture %s missing", kind)
	return nil
}

func marshal(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func validator(t *testing.T) *Validator {
	t.Helper()
	v, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestContractExamples(t *testing.T) {
	v := validator(t)
	valid := fixtures(t)
	if len(valid) != 21 {
		t.Fatalf("want 21 published valid examples, got %d", len(valid))
	}
	for i, raw := range valid {
		t.Run(fmt.Sprintf("valid_%d", i), func(t *testing.T) {
			env, err := v.Validate(raw)
			if err != nil {
				t.Fatal(err)
			}
			if env.Protocol != 1 || env.Type == "" || len(env.Body) == 0 {
				t.Fatal("envelope not decoded")
			}
		})
	}
	b, err := os.ReadFile("../../contracts/examples/agent-v1.invalid.json")
	if err != nil {
		t.Fatal(err)
	}
	var invalid []struct {
		Reason  string
		Message json.RawMessage
	}
	if err := json.Unmarshal(b, &invalid); err != nil {
		t.Fatal(err)
	}
	if len(invalid) != 11 {
		t.Fatalf("want 11 published invalid examples, got %d", len(invalid))
	}
	for _, example := range invalid {
		t.Run(example.Reason, func(t *testing.T) {
			if _, err := v.Validate(example.Message); err == nil {
				t.Fatal("invalid example accepted")
			}
		})
	}
}

func TestRejectMalformedTransportJSON(t *testing.T) {
	v := validator(t)
	hello := marshal(t, fixture(t, "hello"))
	cases := map[string][]byte{
		"empty":                       nil,
		"duplicate root key":          bytes.Replace(hello, []byte(`"protocol":1`), []byte(`"protocol":1,"protocol":1`), 1),
		"duplicate escaped key":       bytes.Replace(hello, []byte(`"protocol":1`), []byte(`"protocol":1,"protoco\u006c":1`), 1),
		"duplicate nested key":        bytes.Replace(hello, []byte(`"policy_revision":1`), []byte(`"policy_revision":1,"policy_revision":1`), 1),
		"invalid utf8":                bytes.Replace(hello, []byte("0.1.0"), []byte{0xff}, 1),
		"trailing object":             append(bytes.Clone(hello), []byte(` {}`)...),
		"trailing scalar":             append(bytes.Clone(hello), []byte(` true`)...),
		"oversized assembled message": append(bytes.Clone(hello), bytes.Repeat([]byte(" "), (8<<20)-len(hello)+1)...),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Validate(raw); err == nil {
				t.Fatal("malformed message accepted")
			}
		})
	}
	boundary := append(bytes.Clone(hello), bytes.Repeat([]byte(" "), (8<<20)-len(hello))...)
	if _, err := v.Validate(boundary); err != nil {
		t.Fatalf("8 MiB boundary rejected: %v", err)
	}
}

func TestActualCalendarDates(t *testing.T) {
	v := validator(t)
	for _, value := range []string{"2026-02-29T04:00:00Z", "2026-04-31T04:00:00Z", "2026-11-04T24:00:00Z", "2026-11-04T04:00:60Z"} {
		msg := fixture(t, "result")
		msg["body"].(map[string]any)["finished_at"] = value
		if _, err := v.Validate(marshal(t, msg)); err == nil {
			t.Errorf("invalid timestamp accepted: %s", value)
		}
	}
	msg := fixture(t, "result")
	msg["body"].(map[string]any)["finished_at"] = "2028-02-29T04:00:00.001Z"
	if _, err := v.Validate(marshal(t, msg)); err != nil {
		t.Fatalf("leap day rejected: %v", err)
	}
}

func prepareFixture(t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	msg := fixture(t, "prepare")
	raw, err := base64.StdEncoding.DecodeString(msg["body"].(map[string]any)["payload_b64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return msg, payload
}

func withPayload(t *testing.T, msg, payload map[string]any) []byte {
	t.Helper()
	msg["body"].(map[string]any)["payload_b64"] = base64.StdEncoding.EncodeToString(marshal(t, payload))
	return marshal(t, msg)
}

func TestPrepareDecodedPayloadValidation(t *testing.T) {
	v := validator(t)
	for name, mutate := range map[string]func(map[string]any){
		"unknown property":   func(p map[string]any) { p["unknown"] = true },
		"missing capability": func(p map[string]any) { p["required_capabilities"] = []string{"compose.apply.v1"} },
		"node mismatch":      func(p map[string]any) { p["node_id"] = "11111111-1111-4111-8111-111111111111" },
		"stack mismatch":     func(p map[string]any) { p["stack_id"] = "11111111-1111-4111-8111-111111111111" },
		"version mismatch":   func(p map[string]any) { p["version_id"] = "11111111-1111-4111-8111-111111111111" },
		"invalid file utf8": func(p map[string]any) {
			p["config"].(map[string]any)["files"].([]any)[0].(map[string]any)["content_b64"] = "/w=="
		},
		"noncanonical file base64": func(p map[string]any) {
			p["config"].(map[string]any)["files"].([]any)[0].(map[string]any)["content_b64"] = "YR=="
		},
		"duplicate file path": func(p map[string]any) {
			c := p["config"].(map[string]any)
			f := c["files"].([]any)
			c["files"] = append(f, f[0])
		},
		"missing compose file": func(p map[string]any) { p["config"].(map[string]any)["compose_files"] = []string{"missing.yaml"} },
		"missing env file": func(p map[string]any) {
			p["config"].(map[string]any)["interpolation_env_files"] = []string{"missing.env"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			msg, payload := prepareFixture(t)
			mutate(payload)
			if _, err := v.Validate(withPayload(t, msg, payload)); err == nil {
				t.Fatal("invalid decoded payload accepted")
			}
		})
	}
	_, payload := prepareFixture(t)
	encoded := marshal(t, payload)
	for name, raw := range map[string][]byte{
		"duplicate JSON key": bytes.Replace(encoded, []byte(`"policy_revision":1`), []byte(`"policy_revision":1,"policy_revision":2`), 1),
		"invalid JSON utf8":  bytes.Replace(encoded, []byte("sample"), []byte{0xff}, 1),
		"trailing JSON":      append(bytes.Clone(encoded), []byte(` {}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			msg, _ := prepareFixture(t)
			msg["body"].(map[string]any)["payload_b64"] = base64.StdEncoding.EncodeToString(raw)
			if _, err := v.Validate(marshal(t, msg)); err == nil {
				t.Fatal("malformed frozen JSON accepted")
			}
		})
	}
}

func TestPrepareFileBudgets(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		name  string
		sizes []int
		want  bool
	}{
		{"exact file limit", []int{1 << 20}, true},
		{"over file limit", []int{(1 << 20) + 1}, false},
		{"exact total limit", []int{1 << 20, 1 << 20, 1 << 20, 1 << 20}, true},
		{"over total limit", []int{1 << 20, 1 << 20, 1 << 20, 1 << 20, 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, payload := prepareFixture(t)
			config := payload["config"].(map[string]any)
			files := make([]any, 0, len(tc.sizes))
			for i, size := range tc.sizes {
				files = append(files, map[string]any{"path": fmt.Sprintf("file%d.yaml", i), "content_b64": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), size))})
			}
			config["files"] = files
			config["compose_files"] = []string{"file0.yaml"}
			_, err := v.Validate(withPayload(t, msg, payload))
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v want=%v (%v)", err == nil, tc.want, err)
			}
		})
	}
}

func TestPortablePathPatternAndByteBudget(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"compose.yaml", true}, {"dir/compose.yaml", true}, {".env", true}, {"..env", true},
		{"/compose.yaml", false}, {"../compose.yaml", false}, {"dir/../compose.yaml", false},
		{"./compose.yaml", false}, {"dir/./file", false}, {"dir/..", false}, {"dir//file", false},
		{"dir/", false}, {"C:file", false}, {`dir\file`, false}, {"dir/\x00file", false},
		{"dir\n/../file", false}, {"dir\n/:file", false},
		{strings.Repeat("界", 80), true}, {strings.Repeat("界", 81), false},
	} {
		t.Run(fmt.Sprintf("%q", tc.path), func(t *testing.T) {
			msg, payload := prepareFixture(t)
			config := payload["config"].(map[string]any)
			config["files"].([]any)[0].(map[string]any)["path"] = tc.path
			config["compose_files"] = []string{tc.path}
			_, err := v.Validate(withPayload(t, msg, payload))
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v want=%v (%v)", err == nil, tc.want, err)
			}
		})
	}
}

func TestSchemaPatternsUseECMAScriptAndPathsHaveRuntimeChecks(t *testing.T) {
	const pathPattern = `^(?!/)(?!.*[\\:\u0000])(?!.*(?:^|/)\.{1,2}(?:/|$))[^/]+(?:/[^/]+)*$`
	re, err := compilePattern(`^(?!private$).+$`)
	if err != nil {
		t.Fatal("ECMAScript lookahead rejected")
	}
	if re.MatchString("private") || !re.MatchString("public") {
		t.Fatal("lookahead semantics changed")
	}
	re, err = compilePattern(pathPattern)
	if err != nil {
		t.Fatal(err)
	}
	// ECMAScript dot does not cross a newline. The schema pattern alone therefore
	// accepts these values; prepare's independent safe-relative-path check rejects
	// them. Do not silently change the schema's accepting set in the regex engine.
	if !re.MatchString("dir\n/../file") || !re.MatchString("dir\n/:file") {
		t.Fatal("schema regex semantics changed")
	}
	if _, err := compilePattern(`[`); err == nil {
		t.Fatal("invalid expression accepted")
	}
}

func TestCanonicalEncodedValues(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct{ kind, key, value string }{
		{"result", "output_b64", "YR=="},
		{"result", "input_digest", strings.Repeat("A", 42) + "B"},
		{"prepare", "payload_b64", "e31="},
	} {
		msg := fixture(t, tc.kind)
		msg["body"].(map[string]any)[tc.key] = tc.value
		if _, err := v.Validate(marshal(t, msg)); err == nil {
			t.Errorf("noncanonical %s accepted", tc.key)
		}
	}
}

func TestValidationErrorsDoNotExposeInput(t *testing.T) {
	v := validator(t)
	secret := "PRIVATE_SECRET_SENTINEL"
	msg := fixture(t, "hello")
	msg["body"].(map[string]any)[secret] = secret
	_, err := v.Validate(marshal(t, msg))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("expected safe validation error, got %v", err)
	}
	msg, payload := prepareFixture(t)
	payload[secret] = secret
	_, err = v.Validate(withPayload(t, msg, payload))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("expected safe payload error, got %v", err)
	}
}

func TestNewMessageUsesNullAndUniqueUUIDv4(t *testing.T) {
	v := validator(t)
	body := fixture(t, "hello")["body"]
	seen := map[string]bool{}
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for range 100 {
		raw, err := NewMessage("hello", "", "", body)
		if err != nil {
			t.Fatal(err)
		}
		env, err := v.Validate(raw)
		if err != nil {
			t.Fatal(err)
		}
		if env.SessionID != nil || env.ReplyTo != nil {
			t.Fatal("empty correlation fields must be null")
		}
		if !pattern.MatchString(env.MessageID) || seen[env.MessageID] {
			t.Fatal("message ID is not a unique UUIDv4")
		}
		seen[env.MessageID] = true
		id, err := NewID()
		if err != nil || !pattern.MatchString(id) || seen[id] {
			t.Fatalf("bad ID: %v", err)
		}
		seen[id] = true
	}
	session, reply := "00000000-0000-4000-8000-000000000008", "00000000-0000-4000-8000-000000000090"
	raw, err := NewMessage("welcome", session, reply, fixture(t, "welcome")["body"])
	if err != nil {
		t.Fatal(err)
	}
	env, err := v.Validate(raw)
	if err != nil {
		t.Fatal(err)
	}
	if env.SessionID == nil || *env.SessionID != session || env.ReplyTo == nil || *env.ReplyTo != reply {
		t.Fatal("message correlation not retained")
	}
	if _, err := NewMessage("hello", "", "", make(chan int)); err == nil {
		t.Fatal("unencodable body accepted")
	}
}

func TestHostileNumberInUniqueItemsCannotPanic(t *testing.T) {
	v := validator(t)
	msg := fixture(t, "hello")
	capabilities := []any{json.Number("1e1000001")}
	for i := range 20 {
		capabilities = append(capabilities, fmt.Sprintf("capability.%d", i))
	}
	msg["body"].(map[string]any)["capabilities"] = capabilities
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("untrusted number caused a panic: %v", recovered)
		}
	}()
	if _, err := v.Validate(marshal(t, msg)); err == nil {
		t.Fatal("hostile numeric token accepted")
	}
}

func TestNumericTokenBudgetsAndExactIntegerSemantics(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		name, token string
		want        bool
	}{
		{"integer", "1", true}, {"decimal integer", "1.0", true},
		{"exponent integer", "1e0", true}, {"scaled integer", "10e-1", true},
		{"maximum safe integer", "9007199254740991", true},
		{"scaled maximum safe integer", "90071992547409910e-1", true},
		{"unsafe integer", "9007199254740992", false},
		{"precision hidden fraction", "1.00000000000000001", false},
		{"near safe limit fraction", "9007199254740990.99999999999", false},
		{"tiny fraction", "1e-128", false}, {"underflow fraction", "1e-1000001", false},
		{"positive exponent boundary zero", "0e128", true},
		{"negative exponent boundary zero", "0e-128", true},
		{"positive exponent over budget zero", "0e129", false},
		{"negative exponent over budget zero", "0e-129", false},
		{"token length boundary zero", "0." + strings.Repeat("0", 126), true},
		{"token length over budget zero", "0." + strings.Repeat("0", 127), false},
		{"negative zero", "-0.0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := fixture(t, "hello")
			msg["body"].(map[string]any)["execution_profile"].(map[string]any)["policy_revision"] = json.Number(tc.token)
			_, err := v.Validate(marshal(t, msg))
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v want=%v (%v)", err == nil, tc.want, err)
			}
		})
	}
	for _, token := range []string{"1.0", "1e0"} {
		msg := fixture(t, "hello")
		msg["protocol"] = json.Number(token)
		env, err := v.Validate(marshal(t, msg))
		if err != nil || env.Protocol != 1 {
			t.Fatalf("equivalent protocol integer rejected: %v", err)
		}
	}
	msg, payload := prepareFixture(t)
	payload["execution_profile"].(map[string]any)["policy_revision"] = json.Number("0e129")
	if _, err := v.Validate(withPayload(t, msg, payload)); err == nil {
		t.Fatal("decoded payload bypassed numeric resource budget")
	}
}
