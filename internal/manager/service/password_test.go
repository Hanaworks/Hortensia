package service

import (
	"os"
	"strings"
	"testing"
)

func TestPasswordLinePreservesMeaning(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{"  password space  \r\n", "  password space  "}, {"secret\nignored", "secret"}, {"no terminator", "no terminator"}, {"literal CR\r", "literal CR\r"}, {"\n", ""}} {
		raw, err := ReadPasswordLine(strings.NewReader(tc.raw))
		if err != nil || string(raw) != tc.want {
			t.Fatalf("input altered: %q %v", raw, err)
		}
	}
	if _, err := ReadPasswordLine(strings.NewReader(strings.Repeat("x", 514))); err == nil {
		t.Fatal("unbounded input accepted")
	}
}
func TestHiddenPasswordRefusesUnprotectedPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w.Close()
	if _, err := ReadHiddenPassword(r); err == nil {
		t.Fatal("pipe silently used as interactive password input")
	}
}
