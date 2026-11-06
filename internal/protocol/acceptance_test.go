package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// These assertions exercise Validate, including its fixed public diagnostics.
// Checking the outer schema separately prevents a malformed envelope or its
// 8 MiB limit from masking a decoded-content rejection.
func requireEnvelopeSchema(t *testing.T, v *Validator, raw []byte) {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("test envelope is not JSON: %v", err)
	}
	if err := v.message.Validate(doc); err != nil {
		t.Fatalf("test envelope violates the outer schema: %v", err)
	}
}

func acceptancePayload(t *testing.T, v *Validator, msg, payload map[string]any) []byte {
	t.Helper()
	raw := withPayload(t, msg, payload)
	if len(raw) > 8<<20 {
		t.Fatalf("decoded-content test exceeds the outer 8 MiB budget: %d", len(raw))
	}
	requireEnvelopeSchema(t, v, raw)
	return raw
}

func requireValidationCode(t *testing.T, v *Validator, raw []byte, want string) {
	t.Helper()
	env, err := v.Validate(raw)
	if want == "" {
		if err != nil {
			t.Fatalf("valid message rejected: %v", err)
		}
		if env.Protocol != 1 || env.Type == "" || len(env.Body) == 0 {
			t.Fatal("accepted message did not return a decoded envelope")
		}
		return
	}
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want fixed diagnostic %q", err, want)
	}
}

// Removing the raw-length check or changing > to >= must break this test.
func TestAcceptanceRawMessageByteBoundary(t *testing.T) {
	v := validator(t)
	hello := marshal(t, fixture(t, "hello"))
	for _, tc := range []struct {
		name string
		size int
		code string
	}{
		{"below", (8 << 20) - 1, ""},
		{"exact", 8 << 20, ""},
		{"above", (8 << 20) + 1, "budget_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := append(bytes.Clone(hello), bytes.Repeat([]byte(" "), tc.size-len(hello))...)
			if len(raw) != tc.size {
				t.Fatalf("test message has %d bytes, want %d", len(raw), tc.size)
			}
			requireEnvelopeSchema(t, v, raw)
			requireValidationCode(t, v, raw, tc.code)
		})
	}
}

// Multibyte content makes counting characters instead of decoded bytes fail.
// 1 MiB + 1 still fits the schema's Base64 character limit of 1398104.
func TestAcceptanceOutputDecodedByteBoundary(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		name string
		size int
		code string
	}{
		{"below", (1 << 20) - 1, ""},
		{"exact", 1 << 20, ""},
		{"above", (1 << 20) + 1, "encoding_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Repeat("界", tc.size/3) + strings.Repeat("a", tc.size%3)
			if len(content) != tc.size || !utf8.ValidString(content) {
				t.Fatal("test output does not have the requested UTF-8 byte length")
			}
			msg := fixture(t, "result")
			msg["body"].(map[string]any)["output_b64"] = base64.StdEncoding.EncodeToString([]byte(content))
			raw := marshal(t, msg)
			if len(raw) > 8<<20 {
				t.Fatal("output test exceeds outer message budget")
			}
			requireEnvelopeSchema(t, v, raw)
			requireValidationCode(t, v, raw, tc.code)
		})
	}
}

// Each file remains within its own schema limit, and every reference exists.
// Removing either decoded-byte budget or changing > to >= breaks these cases.
func TestAcceptancePrepareDecodedFileByteBoundaries(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		name  string
		sizes []int
		code  string
	}{
		{"single_below", []int{(1 << 20) - 1}, ""},
		{"single_exact", []int{1 << 20}, ""},
		{"single_above", []int{(1 << 20) + 1}, "budget_exceeded"},
		{"total_below", []int{1 << 20, 1 << 20, 1 << 20, (1 << 20) - 1}, ""},
		{"total_exact", []int{1 << 20, 1 << 20, 1 << 20, 1 << 20}, ""},
		{"total_above", []int{1 << 20, 1 << 20, 1 << 20, 1 << 20, 1}, "budget_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, payload := prepareFixture(t)
			config := payload["config"].(map[string]any)
			files := make([]any, len(tc.sizes))
			for i, size := range tc.sizes {
				content := strings.Repeat("界", size/3) + strings.Repeat("a", size%3)
				files[i] = map[string]any{
					"path":        fmt.Sprintf("file%d.yaml", i),
					"content_b64": base64.StdEncoding.EncodeToString([]byte(content)),
				}
			}
			config["files"] = files
			config["compose_files"] = []any{"file0.yaml"}
			config["interpolation_env_files"] = []any{}
			if err := v.payload.Validate(payload); err != nil {
				t.Fatalf("test payload must satisfy schema before decoded budgets: %v", err)
			}
			requireValidationCode(t, v, acceptancePayload(t, v, msg, payload), tc.code)
		})
	}
}

// A weakened file-count limit accepts 129; an off-by-one rejects 128.
func TestAcceptancePrepareFileCountBoundary(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		count int
		code  string
	}{
		{127, ""}, {128, ""}, {129, "payload_invalid"},
	} {
		t.Run(fmt.Sprint(tc.count), func(t *testing.T) {
			msg, payload := prepareFixture(t)
			config := payload["config"].(map[string]any)
			files := make([]any, tc.count)
			for i := range files {
				files[i] = map[string]any{"path": fmt.Sprintf("file%d.yaml", i), "content_b64": "YQ=="}
			}
			config["files"] = files
			config["compose_files"] = []string{"file0.yaml"}
			config["interpolation_env_files"] = []string{fmt.Sprintf("file%d.yaml", tc.count-1)}
			requireValidationCode(t, v, acceptancePayload(t, v, msg, payload), tc.code)
		})
	}
}

// All three paths have fewer than 240 characters, so only the runtime UTF-8
// byte limit can reject the 241-byte case, including its existing references.
func TestAcceptancePreparePathUTF8ByteBoundary(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		size int
		path string
		code string
	}{
		{239, strings.Repeat("界", 79) + "aa", ""},
		{240, strings.Repeat("界", 80), ""},
		{241, strings.Repeat("界", 80) + "a", "budget_exceeded"},
	} {
		t.Run(fmt.Sprint(tc.size), func(t *testing.T) {
			if len(tc.path) != tc.size || !utf8.ValidString(tc.path) {
				t.Fatal("test path does not have the requested UTF-8 byte length")
			}
			msg, payload := prepareFixture(t)
			config := payload["config"].(map[string]any)
			config["files"].([]any)[0].(map[string]any)["path"] = tc.path
			config["compose_files"] = []any{tc.path}
			config["interpolation_env_files"] = []any{tc.path}
			if err := v.payload.Validate(payload); err != nil {
				t.Fatalf("test path must satisfy character-based schema: %v", err)
			}
			requireValidationCode(t, v, acceptancePayload(t, v, msg, payload), tc.code)
		})
	}
}

// A missing membership or duplicate-path check would accept these mutations.
// Separate files with the same path have different contents, not equal objects.
func TestAcceptancePrepareReferenceIntegrity(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		code   string
	}{
		{"existing_compose_and_env", func(c map[string]any) {}, ""},
		{"duplicate_file_path", func(c map[string]any) {
			c["files"] = append(c["files"].([]any), map[string]any{"path": "compose.yaml", "content_b64": "Yg=="})
		}, "payload_invalid"},
		{"missing_compose", func(c map[string]any) { c["compose_files"] = []string{"private-missing.yaml"} }, "payload_invalid"},
		{"missing_env", func(c map[string]any) { c["interpolation_env_files"] = []string{"private-missing.env"} }, "payload_invalid"},
		{"duplicate_compose_reference", func(c map[string]any) { c["compose_files"] = []string{"compose.yaml", "compose.yaml"} }, "payload_invalid"},
		{"duplicate_env_reference", func(c map[string]any) { c["interpolation_env_files"] = []string{"private.env", "private.env"} }, "payload_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, payload := prepareFixture(t)
			config := payload["config"].(map[string]any)
			config["files"] = append(config["files"].([]any), map[string]any{"path": "private.env", "content_b64": "U0VDUkVUPXZhbHVl"})
			config["interpolation_env_files"] = []string{"private.env"}
			// Validate the common control before changing only the named condition.
			requireValidationCode(t, v, acceptancePayload(t, v, msg, payload), "")
			tc.mutate(config)
			requireValidationCode(t, v, acceptancePayload(t, v, msg, payload), tc.code)
		})
	}
}

// Dropping any node/stack/version comparison must fail for either side.
func TestAcceptancePrepareTargetsMatchEnvelope(t *testing.T) {
	v := validator(t)
	for _, key := range []string{"node_id", "stack_id", "version_id"} {
		for _, side := range []string{"inner", "outer"} {
			t.Run(key+"/"+side, func(t *testing.T) {
				msg, payload := prepareFixture(t)
				requireValidationCode(t, v, acceptancePayload(t, v, msg, payload), "")
				target := payload
				if side == "outer" {
					target = msg["body"].(map[string]any)
				}
				target[key] = "11111111-1111-4111-8111-111111111111"
				if err := v.payload.Validate(payload); err != nil {
					t.Fatalf("mismatched target must still satisfy payload schema: %v", err)
				}
				requireValidationCode(t, v, acceptancePayload(t, v, msg, payload), "payload_invalid")
			})
		}
	}
}

// Nonzero padding bits satisfy the schema but must fail canonical decoding.
// Each canonical partner is valid; in particular payload_b64 decodes to a full
// valid FrozenPayload, so a payload error cannot mask a Base64 acceptance bug.
func TestAcceptanceCanonicalEncoding(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct{ name, canonical, noncanonical string }{
		{"output", "YQ==", "YR=="},
		{"file", "YQ==", "YR=="},
		{"digest", strings.Repeat("_", 42) + "8", strings.Repeat("_", 42) + "9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, encoding := range []struct{ value, code string }{{tc.canonical, ""}, {tc.noncanonical, "encoding_invalid"}} {
				var raw []byte
				if tc.name == "file" {
					msg, payload := prepareFixture(t)
					payload["config"].(map[string]any)["files"].([]any)[0].(map[string]any)["content_b64"] = encoding.value
					raw = acceptancePayload(t, v, msg, payload)
				} else {
					msg := fixture(t, "result")
					key := "output_b64"
					if tc.name == "digest" {
						key = "input_digest"
					}
					msg["body"].(map[string]any)[key] = encoding.value
					raw = marshal(t, msg)
					requireEnvelopeSchema(t, v, raw)
				}
				requireValidationCode(t, v, raw, encoding.code)
			}
		})
	}
	t.Run("payload", func(t *testing.T) {
		msg, payload := prepareFixture(t)
		decoded := marshal(t, payload)
		for len(decoded)%3 != 1 {
			decoded = append(decoded, ' ')
		}
		canonical := base64.StdEncoding.EncodeToString(decoded)
		// With a one-byte tail the second Base64 digit has four zero pad bits.
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
		last := len(canonical) - 3
		changed := []byte(canonical)
		changed[last] = alphabet[strings.IndexByte(alphabet, canonical[last])+1]
		if permissive, err := base64.StdEncoding.DecodeString(string(changed)); err != nil || !bytes.Equal(permissive, decoded) {
			t.Fatal("noncanonical fixture must permissively decode to the same valid payload")
		}
		for _, encoding := range []struct{ value, code string }{{canonical, ""}, {string(changed), "encoding_invalid"}} {
			msg["body"].(map[string]any)["payload_b64"] = encoding.value
			raw := marshal(t, msg)
			requireEnvelopeSchema(t, v, raw)
			requireValidationCode(t, v, raw, encoding.code)
		}
	})
}

// Syntax errors are rejected at the schema boundary; canonical pad bits are
// tested separately above to ensure the runtime decoder is also exercised.
func TestAcceptanceBase64Syntax(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct{ name, value string }{
		{"missing_padding", "YQ"},
		{"excess_padding", "YQ==="},
		{"url_alphabet_in_standard_base64", "77-_"},
		{"embedded_newline", "YQ\n=="},
	} {
		for _, location := range []string{"output", "file"} {
			t.Run(location+"/"+tc.name, func(t *testing.T) {
				if location == "file" {
					msg, payload := prepareFixture(t)
					payload["config"].(map[string]any)["files"].([]any)[0].(map[string]any)["content_b64"] = tc.value
					requireValidationCode(t, v, acceptancePayload(t, v, msg, payload), "payload_invalid")
				} else {
					msg := fixture(t, "result")
					msg["body"].(map[string]any)["output_b64"] = tc.value
					requireValidationCode(t, v, marshal(t, msg), "schema_invalid")
				}
			})
		}
	}
	for _, tc := range []struct{ name, value string }{
		{"standard_alphabet_in_digest", strings.Repeat("/", 42) + "8"},
		{"padded_digest", strings.Repeat("_", 42) + "8="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := fixture(t, "result")
			msg["body"].(map[string]any)["input_digest"] = tc.value
			requireValidationCode(t, v, marshal(t, msg), "schema_invalid")
		})
	}
}

// Removing decoded UTF-8 checks must accept malformed bytes and fail here.
// U+FFFD itself is valid input and must not be confused with an invalid byte.
func TestAcceptanceDecodedUTF8(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		name string
		raw  []byte
		code string
	}{
		{"valid_multibyte", []byte("private-secret=界\uFFFD"), ""},
		{"valid_standard_base64_alphabet", []byte("\uFFFF"), ""},
		{"invalid_leading_byte", append([]byte("private-secret="), 0xff), "encoding_invalid"},
		{"truncated_multibyte", append([]byte("private-secret="), 0xe7, 0x95), "encoding_invalid"},
	} {
		for _, location := range []string{"output", "file"} {
			t.Run(location+"/"+tc.name, func(t *testing.T) {
				encoded := base64.StdEncoding.EncodeToString(tc.raw)
				var raw []byte
				if location == "file" {
					msg, payload := prepareFixture(t)
					payload["config"].(map[string]any)["files"].([]any)[0].(map[string]any)["content_b64"] = encoded
					raw = acceptancePayload(t, v, msg, payload)
				} else {
					msg := fixture(t, "result")
					msg["body"].(map[string]any)["output_b64"] = encoded
					raw = marshal(t, msg)
					requireEnvelopeSchema(t, v, raw)
				}
				requireValidationCode(t, v, raw, tc.code)
			})
		}
	}
	t.Run("payload_json", func(t *testing.T) {
		msg, payload := prepareFixture(t)
		payload["config"].(map[string]any)["interpolation_env"] = map[string]any{"SECRET": "private-secret=界"}
		requireValidationCode(t, v, acceptancePayload(t, v, msg, payload), "")
		decoded := marshal(t, payload)
		decoded = bytes.Replace(decoded, []byte("界"), []byte{0xff}, 1)
		if utf8.Valid(decoded) {
			t.Fatal("test payload must contain invalid UTF-8")
		}
		msg["body"].(map[string]any)["payload_b64"] = base64.StdEncoding.EncodeToString(decoded)
		raw := marshal(t, msg)
		requireEnvelopeSchema(t, v, raw)
		requireValidationCode(t, v, raw, "invalid_json")
	})
}

// Returning dependency errors or including untrusted values must fail these
// exact diagnostic assertions, even when the input contains a secret marker.
func TestAcceptanceFixedDiagnosticsDoNotExposeInput(t *testing.T) {
	v := validator(t)
	const secret = "PRIVATE_ACCEPTANCE_SECRET"
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any) []byte
		code   string
	}{
		{"json", func(msg map[string]any) []byte {
			return append(marshal(t, msg), []byte(` "`+secret+`"`)...)
		}, "invalid_json"},
		{"schema", func(msg map[string]any) []byte {
			msg["body"].(map[string]any)[secret] = secret
			return marshal(t, msg)
		}, "schema_invalid"},
		{"encoding", func(map[string]any) []byte {
			msg := fixture(t, "result")
			msg["body"].(map[string]any)["output_b64"] = base64.StdEncoding.EncodeToString(append([]byte(secret), 0xff))
			return marshal(t, msg)
		}, "encoding_invalid"},
		{"payload", func(map[string]any) []byte {
			msg, payload := prepareFixture(t)
			payload[secret] = secret
			return acceptancePayload(t, v, msg, payload)
		}, "payload_invalid"},
		{"budget", func(msg map[string]any) []byte {
			msg["body"].(map[string]any)["execution_profile"].(map[string]any)["policy_revision"] = json.Number("0e129")
			return marshal(t, msg)
		}, "budget_exceeded"},
		{"number", func(msg map[string]any) []byte {
			msg["body"].(map[string]any)["execution_profile"].(map[string]any)["policy_revision"] = json.Number("1.00000000000000001")
			return marshal(t, msg)
		}, "number_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := fixture(t, "hello")
			msg["body"].(map[string]any)["agent_version"] = secret
			requireValidationCode(t, v, marshal(t, msg), "")
			requireValidationCode(t, v, tc.mutate(msg), tc.code)
		})
	}
}
