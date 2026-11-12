package compose

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

func TestDotenvEveryOperatorBranchIsInspected(t *testing.T) {
	for _, operator := range []string{"-", ":-", "+", ":+", "?", ":?"} {
		for _, outer := range []string{"MISSING", "M1_CLI_FIXTURE", "EMPTY"} {
			text := "SAFE=${" + outer + operator + "${INNER:-${HOME}}}\n"
			if err := validateInterpolationEnv([]byte(text)); !errors.Is(err, errEnvControl) {
				t.Errorf("operator %s branch %s: expected control-key rejection, got %v", operator, outer, err)
			}
		}
	}
}

func TestDotenvQuotesCommentsAndEscapes(t *testing.T) {
	allowed := []string{
		`SAFE='${DOCKER_HOST}'`,
		"SAFE='first\n${HOME}'\n",
		`SAFE="\${HOME}"`,
		`SAFE="$${HOME}"`,
		`SAFE=$${DOCKER_HOST}`,
		`SAFE="\0044\0044{HOME}"`,
		"SAFE=literal # ${DOCKER_HOST}\n",
		"# ${HOME}\nSAFE=value\n",
		"SAFE='${HOME}' # ${PATH}\nNEXT=value\n",
		"\xef\xbb\xbfexport SAFE='literal $PATH'\r\nNEXT=value\r\n",
		`SAFE="literal \x24{HOME}"`,
		`SAFE=word'without-lookups`,
	}
	for i, text := range allowed {
		if err := validateInterpolationEnv([]byte(text)); err != nil {
			t.Errorf("literal corpus %d rejected: %v", i, err)
		}
	}
	for i, text := range []string{
		`SAFE=\${HOME}`,
		`SAFE=prefix'${HOME}'`,
		`SAFE="\0044{MISSING-\0044{HOME}}"`,
		`SAFE="escaped \" quote ${HOME}"`,
		`SAFE="\\${HOME}"`,
		`SAFE=$${OUTER-${HOME}}`,
		"SAFE='literal' NEXT=${MISSING:-${HOME}}\n",
		"SAFE=literal#${HOME}\n",
		"SAFE=\"first\n${MISSING:+${PATH}}\"\n",
	} {
		if err := validateInterpolationEnv([]byte(text)); !errors.Is(err, errEnvControl) {
			t.Errorf("active corpus %d escaped control check: %v", i, err)
		}
	}
}

func TestDoubleQuoteEscapeBoundaryMatchesPinnedDotenv(t *testing.T) {
	// The mature parser is the oracle for the escape forms that introduce dollars.
	// An actually visited forbidden name must always be rejected by the safety pass.
	for _, text := range []string{`SAFE="\0044{HOME}"`, `SAFE="\\${HOME}"`, `SAFE="\${HOME}"`, `SAFE="\0044\0044{HOME}"`, `SAFE="\x24{HOME}"`} {
		visited := false
		_, err := dotenv.ParseWithLookup(bytes.NewReader([]byte(text)), func(key string) (string, bool) {
			if controlKey(key) {
				visited = true
			}
			return "test-value", true
		})
		if err != nil {
			t.Fatal("oracle fixture is not valid dotenv")
		}
		check := validateInterpolationEnv([]byte(text))
		if visited != errors.Is(check, errEnvControl) {
			t.Errorf("escape corpus disagrees with mature parser; visited=%v check=%v", visited, check)
		}
	}
}

func TestDotenvMalformedAndBudgetRemainRejected(t *testing.T) {
	for _, text := range []string{"SAFE='unterminated", "SAFE=\"unterminated", strings.Repeat("x", (1<<20)+1)} {
		if err := validateInterpolationEnv([]byte(text)); !errors.Is(err, errEnvSyntax) {
			t.Errorf("invalid dotenv accepted: %v", err)
		}
	}
}
