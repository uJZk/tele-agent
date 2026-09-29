package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/ujzk/tele-agent/internal/client"
	"github.com/ujzk/tele-agent/internal/preflight"
)

func TestApparmorProfile(t *testing.T) {
	p := ApparmorProfile(`/home/a b/.local/bin/te*le`)
	if !strings.Contains(p, `profile tele "/home/a b/.local/bin/te\*le" flags=(unconfined) {`) || !strings.Contains(p, "  userns,\n") {
		t.Fatalf("profile:\n%s", p)
	}
	if strings.Contains(p, "@TELE@") {
		t.Fatal("placeholder left in the profile")
	}
}

func TestSkew(t *testing.T) {
	for skew, want := range map[time.Duration]preflight.Status{
		time.Second:       preflight.OK,
		-12 * time.Second: preflight.Warn,
		31 * time.Second:  preflight.Fatal,
	} {
		if got := skewResult(&client.Session{ClockSkew: skew}).Status; got != want {
			t.Errorf("skew %v: status %v, want %v", skew, got, want)
		}
	}
}
