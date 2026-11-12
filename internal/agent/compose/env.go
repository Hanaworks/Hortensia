package compose

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

var errEnvControl = errors.New("execution_environment_key_forbidden")
var errEnvSyntax = errors.New("interpolation_env_invalid")

// validateInterpolationEnv never supplies execution values. dotenv remains the
// syntax/value parser; the separate lexical pass inspects every potential lookup
// before a lazy default/alternative/required operator can skip its operand.
func validateInterpolationEnv(raw []byte) error {
	if len(raw) > 1<<20 {
		return errEnvSyntax
	}
	if err := scanEnvReferences(string(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))); err != nil {
		return err
	}
	bad := false
	values, err := dotenv.ParseWithLookup(bytes.NewReader(raw), func(key string) (string, bool) {
		if controlKey(key) {
			bad = true
		}
		// Nonempty placeholders prevent missing-value warnings and false errors
		// for required expressions. They are discarded, never passed to the CLI.
		return "agent-syntax-check", true
	})
	if bad {
		return errEnvControl
	}
	if err != nil {
		return errEnvSyntax
	}
	for key := range values {
		if controlKey(key) {
			return errEnvControl
		}
	}
	return nil
}

// scanEnvReferences identifies dotenv statement/value boundaries only. It does
// not evaluate an operator, assign a variable, or choose a conditional branch.
// These boundaries follow compose-go v2.14.0 dotenv/parser.go; dependency changes
// must recheck comments, quoting, and escape rules with the corpus in env_test.go.
func scanEnvReferences(src string) error {
	for len(src) > 0 {
		src = strings.TrimLeftFunc(src, unicode.IsSpace)
		if src == "" {
			return nil
		}
		if src[0] == '#' {
			if end := strings.IndexByte(src, '\n'); end >= 0 {
				src = src[end+1:]
				continue
			}
			return nil
		}
		at := strings.IndexAny(src, "=:\n")
		if at < 0 {
			return nil
		} // dotenv validates bare/incomplete declarations.
		inherited := src[at] == '\n'
		src = src[at+1:]
		if inherited {
			continue
		}
		src = strings.TrimLeftFunc(src, dotenvSpace)
		if src == "" {
			return nil
		}
		if src[0] != '\'' && src[0] != '"' {
			value, rest, _ := strings.Cut(src, "\n")
			value, _, _ = strings.Cut(value, " #")
			if err := scanVariableKeys(value); err != nil {
				return err
			}
			src = rest
			continue
		}
		quote := src[0]
		end := 1
		for end < len(src) {
			if src[end] == '\\' && end+1 < len(src) {
				end += 2
				continue
			}
			if src[end] == quote {
				break
			}
			end++
		}
		if end == len(src) {
			return errEnvSyntax
		}
		if quote == '"' {
			if err := scanVariableKeys(decodeEnvDoubleQuotes(src[1:end])); err != nil {
				return err
			}
		}
		// Single-quoted values do not interpolate, including multiline values.
		src = src[end+1:]
	}
	return nil
}

func dotenvSpace(r rune) bool {
	switch r {
	case '\t', '\v', '\f', '\r', ' ', 0x85, 0xa0:
		return true
	}
	return false
}

// Only the supported double-quote escape forms can change which dollars reach
// the native template parser. In particular \$ becomes its literal $$ form,
// while an octal escape may introduce an active dollar. Unsupported Go escapes
// (such as \x24) remain literal, as they do in the pinned dotenv library.
func decodeEnvDoubleQuotes(src string) string {
	var out strings.Builder
	for i := 0; i < len(src); {
		if src[i] != '\\' || i+1 == len(src) {
			out.WriteByte(src[i])
			i++
			continue
		}
		next := src[i+1]
		if next == '$' {
			out.WriteString("$$")
			i += 2
			continue
		}
		end := i + 2
		match := src[i:end]
		if next == '0' {
			for end < len(src) && end < i+5 && src[end] >= '0' && src[end] <= '9' {
				end++
			}
			match = "\\" + src[i+2:end]
		} else if !strings.ContainsRune("abcfnrtv\"\\", rune(next)) {
			// The dotenv quote locator also removes a slash before an escaped
			// matching quote. All other unsupported sequences stay unchanged.
			out.WriteByte(src[i])
			i++
			continue
		}
		r, _, tail, err := strconv.UnquoteChar(match, '"')
		if err == nil && tail == "" {
			out.WriteRune(r)
		} else {
			out.WriteString(src[i:end])
		}
		i = end
	}
	return out.String()
}

func variableStart(b byte) bool { return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }
func variablePart(b byte) bool  { return variableStart(b) || b >= '0' && b <= '9' }

func scanVariableKeys(value string) error {
	for at := 0; at < len(value); {
		if value[at] != '$' {
			_, n := utf8.DecodeRuneInString(value[at:])
			at += n
			continue
		}
		at++
		if at == len(value) {
			break
		}
		if value[at] == '$' {
			at++
			continue
		}
		if value[at] == '{' {
			at++
		}
		start := at
		if at >= len(value) || !variableStart(value[at]) {
			continue
		}
		for at < len(value) && variablePart(value[at]) {
			at++
		}
		if controlKey(value[start:at]) {
			return errEnvControl
		}
		// Continue lexically through every operand, including required messages,
		// rather than deciding whether the current lookup would evaluate it.
	}
	return nil
}
