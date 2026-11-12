package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"docker-manager/contracts"
	"github.com/dlclark/regexp2"
)

func bundledBase64Expression(t *testing.T) string {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(contracts.AgentV1Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	return schema["$defs"].(map[string]any)["Base64"].(map[string]any)["pattern"].(string)
}

// Exercise the user-visible 4 MiB file budget while the matching goroutine
// competes for CPU. There is no speed threshold: scheduling must not change
// whether this supported configuration is accepted.
func TestBase64PrepareCapacityUnderContention(t *testing.T) {
	v := validator(t)
	msg, payload := prepareFixture(t)
	config := payload["config"].(map[string]any)
	files := make([]any, 0, 4)
	for i := 0; i < 4; i++ {
		files = append(files, map[string]any{"path": fmt.Sprintf("file%d.yaml", i), "content_b64": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), 1<<20))})
	}
	config["files"] = files
	config["compose_files"] = []string{"file0.yaml"}
	wire := withPayload(t, msg, payload)
	if len(wire) >= 8<<20 {
		t.Fatalf("invalid fixture exceeds envelope budget: %d", len(wire))
	}
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	stop := make(chan struct{})
	var workers sync.WaitGroup
	defer func() { close(stop); workers.Wait() }()
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
	}
	for attempt := 0; attempt < 2; attempt++ {
		got, err := v.Validate(wire)
		if err != nil || got.Type != "prepare" {
			t.Fatalf("legal 4 MiB configuration rejected under contention (attempt %d): %v", attempt, err)
		}
	}
}

// The oracle remains the unchanged shared Schema expression interpreted by
// the old ECMAScript engine. Inputs are short, so its timeout is not a budget
// for this differential check. Enumeration covers valid and invalid forms.
func TestBase64PatternMatchesECMAScript(t *testing.T) {
	expression := bundledBase64Expression(t)
	candidate, err := compilePattern(expression)
	if err != nil {
		t.Fatal(err)
	}
	oracle, err := regexp2.Compile(expression, regexp2.ECMAScript)
	if err != nil {
		t.Fatal(err)
	}
	oracle.MatchTimeout = 100 * time.Millisecond
	compare := func(value string) {
		t.Helper()
		want, err := oracle.MatchString(value)
		if err != nil {
			t.Fatalf("oracle failed on %q: %v", value, err)
		}
		if got := candidate.MatchString(value); got != want {
			t.Fatalf("Base64 syntax changed for %q: got=%v want=%v", value, got, want)
		}
	}
	for _, seed := range []string{"", "AA==", "AAA=", "AAAA", "AA==\n", "AAAAAAAA", "+/z9", "Zg==", "Zh==", "Zm9="} {
		compare(seed)
		for position := 0; position <= len(seed); position++ {
			for b := 0; b < 256; b++ {
				compare(seed[:position] + string([]byte{byte(b)}) + seed[position:])
			}
		}
		for _, suffix := range []string{"\n", "\n\n", "\r", "\r\n", "\u0085", "\u2028", "\u2029"} {
			compare(seed + suffix)
		}
	}
	alphabet := []byte{'A', '/', '=', '\n', '\r'}
	var walk func([]byte, int)
	walk = func(prefix []byte, remaining int) {
		compare(string(prefix))
		if remaining == 0 {
			return
		}
		for _, b := range alphabet {
			walk(append(prefix, b), remaining-1)
		}
	}
	walk(nil, 6)
	for size := 0; size <= 256; size++ {
		canonical := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{byte(size)}, size))
		compare(canonical)
		compare(canonical + "\n")
		compare(canonical + "=")
		if len(canonical) > 0 {
			compare(canonical[:len(canonical)-1])
		}
	}
}

func TestBase64SyntaxAndCanonicalEncodingRemainDistinct(t *testing.T) {
	v := validator(t)
	syntax, err := compilePattern(bundledBase64Expression(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, value      string
		syntaxOK, wireOK bool
	}{
		{"empty", "", true, true},
		{"one byte", "YQ==", true, true},
		{"two bytes", "YWI=", true, true},
		{"full group", "YWJj", true, true},
		{"invalid byte", "Y@==", false, false},
		{"unpadded", "YQ", false, false},
		{"too much padding", "Y===", false, false},
		{"embedded padding", "Y=Q=", false, false},
		{"after padding", "YQ==AAAA", false, false},
		{"internal newline", "YQ\n==", false, false},
		{"final LF", "YQ==\n", false, false},
		{"empty final LF", "\n", false, false},
		{"two final LF", "YQ==\n\n", false, false},
		{"final CRLF", "YQ==\r\n", false, false},
		{"one byte noncanonical pad bits", "YR==", true, false},
		{"two bytes noncanonical pad bits", "YWJ=", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := syntax.MatchString(c.value); got != c.syntaxOK {
				t.Fatalf("syntax accepted=%v want=%v", got, c.syntaxOK)
			}
			msg := fixture(t, "result")
			msg["body"].(map[string]any)["output_b64"] = c.value
			_, err := v.Validate(marshal(t, msg))
			if (err == nil) != c.wireOK {
				t.Fatalf("wire accepted=%v want=%v (%v)", err == nil, c.wireOK, err)
			}
		})
	}
}

func TestBase64SpecializationKeepsOtherPatternRules(t *testing.T) {
	re, err := compilePattern(`^(?!private$).+$`)
	if err != nil {
		t.Fatal(err)
	}
	if re.MatchString("private") || !re.MatchString("public") {
		t.Fatal("ECMAScript lookahead semantics changed")
	}
	bounded, ok := re.(schemaPattern)
	if !ok || bounded.re.MatchTimeout != 100*time.Millisecond {
		t.Fatal("non-Base64 expressions lost original timeout policy")
	}
	// Ensure the exception is exact: a related but different expression still
	// goes through the original parser and keeps its own accepting language.
	re, err = compilePattern(`^[A-Za-z0-9+/]+$`)
	if err != nil {
		t.Fatal(err)
	}
	if !re.MatchString("A") || re.MatchString("AA==") {
		t.Fatal("different Base64-like syntax was replaced")
	}
	if _, err = compilePattern(`[`); err == nil {
		t.Fatal("invalid general expression accepted")
	}
}
