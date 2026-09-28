package cli

import (
	"path"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in      string
		want    Target
		wantErr bool
	}{
		{in: "dev", want: Target{Alias: "dev"}},
		{in: "dev:", want: Target{Alias: "dev"}},
		{in: "dev:proj", want: Target{Alias: "dev", Dir: "proj"}},
		{in: "dev:proj/", want: Target{Alias: "dev", Dir: "proj"}},
		{in: "dev:/srv/app", want: Target{Alias: "dev", Dir: "/srv/app"}},
		{in: "dev:/srv/app//", want: Target{Alias: "dev", Dir: "/srv/app"}},
		{in: "dev:/", want: Target{Alias: "dev", Dir: "/"}},
		{in: "dev://", want: Target{Alias: "dev", Dir: "/"}},
		{in: "dev:a:b", want: Target{Alias: "dev", Dir: "a:b"}},
		{in: "dev::", want: Target{Alias: "dev", Dir: ":"}},
		{in: "dev:with space", want: Target{Alias: "dev", Dir: "with space"}},
		{in: "my-host.example_1", want: Target{Alias: "my-host.example_1"}},
		{in: "0", want: Target{Alias: "0"}},
		{in: "Host", want: Target{Alias: "Host"}}, // reserved words are exact
		{in: "hosts:x", want: Target{Alias: "hosts", Dir: "x"}},
		{in: "", wantErr: true},
		{in: ":proj", wantErr: true},
		{in: "-dev", wantErr: true},
		{in: ".dev", wantErr: true},
		{in: "_dev", wantErr: true},
		{in: "de v", wantErr: true},
		{in: "dé", wantErr: true},
		{in: "dev/x:y", wantErr: true},
		{in: "user@dev", wantErr: true},
		{in: "host", wantErr: true},
		{in: "doctor:proj", wantErr: true},
		{in: "server", wantErr: true},
		{in: "help", wantErr: true},
		{in: "version:/", wantErr: true},
		{in: "dev:/a\x00b", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseTarget(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseTarget(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestIsReserved(t *testing.T) {
	for _, w := range []string{"host", "doctor", "server", "help", "version"} {
		if !IsReserved(w) {
			t.Errorf("IsReserved(%q) = false", w)
		}
		if err := CheckAlias(w); err == nil {
			t.Errorf("CheckAlias(%q) accepted a reserved word", w)
		}
	}
	for _, w := range []string{"dev", "HOST", "hosts", ""} {
		if IsReserved(w) {
			t.Errorf("IsReserved(%q) = true", w)
		}
	}
}

func TestResolveDir(t *testing.T) {
	tests := []struct {
		dir, home string
		want      string
		wantErr   bool
	}{
		{dir: "", home: "/home/bob", want: "/home/bob"},
		{dir: "", home: "/home/bob/", want: "/home/bob"},
		{dir: "proj", home: "/home/bob", want: "/home/bob/proj"},
		{dir: "a/./b/../c", home: "/home/bob", want: "/home/bob/a/c"},
		{dir: "../alice", home: "/home/bob", want: "/home/alice"},
		{dir: "../../../..", home: "/home/bob", want: "/"},
		{dir: "/srv//app/.", home: "/home/bob", want: "/srv/app"},
		{dir: "/", home: "/home/bob", want: "/"},
		{dir: "a:b", home: "/", want: "/a:b"},
		{dir: "proj", home: "home/bob", wantErr: true},
		{dir: "proj", home: "", wantErr: true},
		{dir: "a\x00b", home: "/home/bob", wantErr: true},
		{dir: "", home: "/home/\x00bob", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ResolveDir(tt.dir, tt.home)
		if (err != nil) != tt.wantErr {
			t.Errorf("ResolveDir(%q, %q) err = %v, wantErr %v", tt.dir, tt.home, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ResolveDir(%q, %q) = %q, want %q", tt.dir, tt.home, got, tt.want)
		}
	}
}

func FuzzParseTarget(f *testing.F) {
	for _, s := range []string{"dev", "dev:proj/", "dev:/", "dev:a:b", ":x", "host", "a.b-c_d:/x//", "dev:\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		tgt, err := ParseTarget(s)
		if err != nil {
			return
		}
		if CheckAlias(tgt.Alias) != nil || strings.Contains(tgt.Alias, ":") {
			t.Fatalf("ParseTarget(%q) returned invalid alias %q", s, tgt.Alias)
		}
		if !strings.HasPrefix(s, tgt.Alias) {
			t.Fatalf("ParseTarget(%q) alias %q is not a prefix", s, tgt.Alias)
		}
		if rest := s[len(tgt.Alias):]; rest != "" && !strings.HasPrefix(rest[1:], tgt.Dir) {
			t.Fatalf("ParseTarget(%q) dir %q is not the text after ':'", s, tgt.Dir)
		}
		if tgt.Dir != "/" && strings.HasSuffix(tgt.Dir, "/") {
			t.Fatalf("ParseTarget(%q) dir %q keeps a trailing slash", s, tgt.Dir)
		}
		got, err := ResolveDir(tgt.Dir, "/home/u")
		if err != nil {
			t.Fatalf("ResolveDir(%q) after successful parse: %v", tgt.Dir, err)
		}
		if !path.IsAbs(got) || path.Clean(got) != got {
			t.Fatalf("ResolveDir(%q) = %q, not absolute and clean", tgt.Dir, got)
		}
	})
}

func FuzzResolveDir(f *testing.F) {
	f.Add("", "/home/u")
	f.Add("../..", "/")
	f.Add("/x/./y/", "/root")
	f.Add("p", "rel")
	f.Fuzz(func(t *testing.T, dir, home string) {
		got, err := ResolveDir(dir, home)
		if err != nil {
			return
		}
		if !path.IsAbs(got) || path.Clean(got) != got || strings.IndexByte(got, 0) >= 0 {
			t.Fatalf("ResolveDir(%q, %q) = %q, not absolute and clean", dir, home, got)
		}
		if dir == "" && got != path.Clean(home) {
			t.Fatalf("ResolveDir(\"\", %q) = %q, want home", home, got)
		}
	})
}
