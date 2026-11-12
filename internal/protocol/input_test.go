package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestSharedInputRejectsUnpairedSurrogatesBeforeReplacement(t *testing.T) {
	for _, raw := range []string{`"\ud800"`, `"\udfff"`, `"\udc00\ud800"`, `"\ud800x\udc00"`, `"\ud800\\udc00"`, `{"\ud800":"value"}`} {
		t.Run(raw, func(t *testing.T) {
			value, err := strictJSON([]byte(raw))
			if err == nil || value != nil {
				t.Fatalf("invalid Unicode was repaired and accepted: value=%v error=%v", value, err)
			}
		})
	}
}

func TestSharedInputStrictJSONValuesAndBudgets(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want any
	}{
		{`"\uD83D\uDE00"`, "😀"}, {`"\ud800\udc00"`, "𐀀"}, {`"\uDBFF\uDFFF"`, string(rune(0x10ffff))},
		{`"\uFFFD"`, "�"}, {`"�"`, "�"}, {`"\\uD800"`, `\uD800`}, {`"text\"\\\n"`, "text\"\\\n"},
		{`1.0`, json.Number("1")}, {`1e0`, json.Number("1")}, {`-0.0`, json.Number("0")}, {`9007199254740991`, json.Number("9007199254740991")}, {`null`, nil},
	} {
		got, err := StrictJSON([]byte(c.raw))
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("parse %q: got=%#v want=%#v err=%v", c.raw, got, c.want, err)
		}
	}
	invalid := []string{
		`{"a":1,"\u0061":2}`, `{"😀":1,"\uD83D\uDE00":2}`, `{} []`, "\ufeff{}", string([]byte{'"', 0xff, '"'}),
		`"\ud800"`, `"\udc00"`, `"\uD800\uD800"`, `"\ud800x"`, `"\u12xz"`, `"\u123"`, `"\uD800\u12xz"`,
		`9007199254740992`, `1.00000000000000001`, `1e-128`, `0e129`, "0." + strings.Repeat("0", 127),
		strings.Repeat("[", 10001) + "0" + strings.Repeat("]", 10001),
	}
	for i, raw := range invalid {
		got, err := StrictJSON([]byte(raw))
		if err == nil || got != nil {
			t.Fatalf("invalid input %d returned tree: %#v %v", i, got, err)
		}
		if strings.Contains(err.Error(), raw) {
			t.Fatalf("error exposed input %d", i)
		}
	}
	bounded := append([]byte("0"), bytes.Repeat([]byte{' '}, (8<<20)-1)...)
	got, err := StrictJSON(bounded)
	if err != nil || got != json.Number("0") {
		t.Fatalf("exact JSON byte budget: %#v %v", got, err)
	}
	if got, err = StrictJSON(append(bounded, ' ')); !errors.Is(err, errBudget) || got != nil {
		t.Fatalf("oversized JSON: %#v %v", got, err)
	}
}

func configFixture(t *testing.T) (map[string]any, map[string]any, map[string]any) {
	t.Helper()
	msg, payload := prepareFixture(t)
	return msg, payload, payload["config"].(map[string]any)
}
func replaceConfigFiles(config map[string]any, sizes ...int) {
	files := make([]any, 0, len(sizes))
	for i, size := range sizes {
		files = append(files, map[string]any{"path": fmt.Sprintf("file%d.yaml", i), "content_b64": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'a'}, size))})
	}
	config["files"] = files
	config["compose_files"] = []string{"file0.yaml"}
	config["interpolation_env_files"] = []string{}
}
func TestSharedInputConfigMatchesPrepare(t *testing.T) {
	v := validator(t)
	cases := []struct {
		name   string
		valid  bool
		mutate func(map[string]any)
	}{
		{"valid", true, func(map[string]any) {}},
		{"environment field names are not protocol", true, func(c map[string]any) {
			c["interpolation_env"] = map[string]any{"input_digest": "not-a-digest", "payload_b64": "not-base64"}
		}},
		{"unknown key", false, func(c map[string]any) { c["extra"] = true }},
		{"missing field", false, func(c map[string]any) { delete(c, "profiles") }},
		{"null field", false, func(c map[string]any) { c["files"] = nil }},
		{"numeric file", false, func(c map[string]any) { c["files"] = []any{json.Number("1.0")} }},
		{"unsafe number", false, func(c map[string]any) {
			c["interpolation_env"] = map[string]any{"BAD": json.Number("9007199254740992")}
		}},
		{"exact file", true, func(c map[string]any) { replaceConfigFiles(c, 1<<20) }},
		{"over file", false, func(c map[string]any) { replaceConfigFiles(c, (1<<20)+1) }},
		{"exact total", true, func(c map[string]any) { replaceConfigFiles(c, 1<<20, 1<<20, 1<<20, 1<<20) }},
		{"over total", false, func(c map[string]any) { replaceConfigFiles(c, 1<<20, 1<<20, 1<<20, 1<<20, 1) }},
		{"noncanonical base64", false, func(c map[string]any) { c["files"].([]any)[0].(map[string]any)["content_b64"] = "YR==" }},
		{"bad UTF8 file", false, func(c map[string]any) { c["files"].([]any)[0].(map[string]any)["content_b64"] = "/w==" }},
		{"unsafe path", false, func(c map[string]any) { c["files"].([]any)[0].(map[string]any)["path"] = "dir/../file" }},
		{"path newline bypass", false, func(c map[string]any) { c["files"].([]any)[0].(map[string]any)["path"] = "dir\n/../file" }},
		{"240 byte path", true, func(c map[string]any) {
			p := strings.Repeat("界", 80)
			c["files"].([]any)[0].(map[string]any)["path"] = p
			c["compose_files"] = []string{p}
		}},
		{"243 byte path", false, func(c map[string]any) {
			p := strings.Repeat("界", 81)
			c["files"].([]any)[0].(map[string]any)["path"] = p
			c["compose_files"] = []string{p}
		}},
		{"duplicate paths", false, func(c map[string]any) { files := c["files"].([]any); c["files"] = append(files, files[0]) }},
		{"missing compose reference", false, func(c map[string]any) { c["compose_files"] = []string{"missing.yaml"} }},
		{"missing env reference", false, func(c map[string]any) { c["interpolation_env_files"] = []string{"missing.env"} }},
		{"duplicate reference", false, func(c map[string]any) {
			p := c["files"].([]any)[0].(map[string]any)["path"].(string)
			c["compose_files"] = []string{p, p}
		}},
		{"128 files", true, func(c map[string]any) { replaceConfigFiles(c, make([]int, 128)...) }},
		{"129 files", false, func(c map[string]any) { replaceConfigFiles(c, make([]int, 129)...) }},
		{"1024 environment values", true, func(c map[string]any) {
			env := map[string]any{}
			for i := 0; i < 1024; i++ {
				env[fmt.Sprintf("KEY_%d", i)] = "value"
			}
			c["interpolation_env"] = env
		}},
		{"1025 environment values", false, func(c map[string]any) {
			env := map[string]any{}
			for i := 0; i < 1025; i++ {
				env[fmt.Sprintf("KEY_%d", i)] = "value"
			}
			c["interpolation_env"] = env
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, payload, config := configFixture(t)
			c.mutate(config)
			raw := marshal(t, config)
			saved := bytes.Clone(raw)
			direct := v.ValidateConfig(raw)
			_, prepare := v.Validate(withPayload(t, msg, payload))
			if (direct == nil) != c.valid || (prepare == nil) != c.valid {
				t.Fatalf("direct=%v prepare=%v wantValid=%v", direct, prepare, c.valid)
			}
			if !bytes.Equal(raw, saved) {
				t.Fatal("validator mutated Config bytes")
			}
		})
	}
}

func TestSharedInputConfigRejectsUnvalidatedShapesAndRawJSON(t *testing.T) {
	v := validator(t)
	for _, raw := range []string{`null`, `[]`, `0`, `"config"`, `{}`, `{"files":[],"files":[]}`, `{} {}`, "\ufeff{}", `{"profiles":["\ud800"]}`} {
		if err := v.ValidateConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid Config %q accepted", raw)
		}
	}
	if err := v.ValidateConfig(bytes.Repeat([]byte{' '}, (8<<20)+1)); !errors.Is(err, errBudget) {
		t.Fatalf("Config raw budget: %v", err)
	}
}

func TestSharedInputUnicodeTighteningReachesProtocolAndPayload(t *testing.T) {
	v := validator(t)
	for _, escaped := range []string{`\uD800`, `\uDC00`, `\uD800x\uDC00`, `\uD83D\uDE00`} {
		valid := escaped == `\uD83D\uDE00`
		// Error messages are arbitrary strings in the shared result body.
		msg := fixture(t, "result")
		body := msg["body"].(map[string]any)
		body["outcome"] = "not_started"
		body["event_seq"] = 1
		body["not_started"] = true
		body["exit_code"] = nil
		body["ownership_status"] = "not_checked"
		body["observation"] = nil
		body["error"] = map[string]any{"category": "execution_failed", "code": "preflight_failed", "message": "UNICODE_SENTINEL", "retry_action": "query_operation"}
		raw := bytes.Replace(marshal(t, msg), []byte("UNICODE_SENTINEL"), []byte(escaped), 1)
		_, err := v.Validate(raw)
		if (err == nil) != valid {
			t.Fatalf("outer Unicode %q: %v", escaped, err)
		}
		prepare, payload, config := configFixture(t)
		config["interpolation_env"] = map[string]any{"VALUE": "UNICODE_SENTINEL"}
		frozen := bytes.Replace(marshal(t, payload), []byte("UNICODE_SENTINEL"), []byte(escaped), 1)
		prepare["body"].(map[string]any)["payload_b64"] = base64.StdEncoding.EncodeToString(frozen)
		_, err = v.Validate(marshal(t, prepare))
		if (err == nil) != valid {
			t.Fatalf("inner Unicode %q: %v", escaped, err)
		}
		direct := bytes.Replace(marshal(t, config), []byte("UNICODE_SENTINEL"), []byte(escaped), 1)
		if err = v.ValidateConfig(direct); (err == nil) != valid {
			t.Fatalf("Config Unicode %q: %v", escaped, err)
		}
	}
}

func TestSharedInputConcurrentConfigValidation(t *testing.T) {
	v := validator(t)
	_, _, config := configFixture(t)
	raw := marshal(t, config)
	invalid := []byte(`{"secret":"\uD800"}`)
	var workers sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for n := 0; n < 8; n++ {
				if err := v.ValidateConfig(raw); err != nil {
					failures <- err
					return
				}
				if _, err := StrictJSON(invalid); err == nil {
					failures <- errors.New("unpaired surrogate accepted")
					return
				}
			}
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}
