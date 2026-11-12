package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestHelpAndVersionDoNotRequireIdentity(t *testing.T) {
	for _, a := range [][]string{{"version"}, {"--version"}, {"serve", "--help"}, {"enroll", "--help"}} {
		var out bytes.Buffer
		if e := run(a, strings.NewReader(""), &out); e != nil || out.Len() == 0 {
			t.Fatal("help/version tried identity", a, e)
		}
	}
}
func TestCLIRejectsSecretsAndUnexpectedFlags(t *testing.T) {
	for _, a := range [][]string{{"enroll", "--token", "secret"}, {"serve", "--node-id", "abc"}, {"init", "--token-stdin"}, {"unknown"}} {
		if e := run(a, strings.NewReader(""), &bytes.Buffer{}); e == nil {
			t.Fatal("accepted invalid CLI", a)
		}
	}
}
