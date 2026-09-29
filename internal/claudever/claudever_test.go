package claudever

import (
	"errors"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.284 (Claude Code)\n": "2.1.284",
		"10.0.1\n":                "10.0.1",
	} {
		if got, err := Parse(in); err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "Claude Code 2.1.284", "2.1"} {
		if _, err := Parse(in); !errors.Is(err, ErrNoVersion) {
			t.Errorf("Parse(%q) err = %v, want ErrNoVersion", in, err)
		}
	}
}

func TestWarning(t *testing.T) {
	if w := Warning(Verified[0]); w != "" {
		t.Errorf("Warning(verified) = %q", w)
	}
	if w := Warning("0.0.1"); !strings.Contains(w, "0.0.1") || !strings.Contains(w, "not been verified") {
		t.Errorf("Warning(unverified) = %q", w)
	}
}
