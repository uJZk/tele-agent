package proto

import (
	"bytes"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFrameRoundTrip(t *testing.T) {
	msgs := []any{
		&StreamHeader{Kind: StreamExec},
		&Hello{Version: Version, Token: []byte{1, 2, 3}, SessionID: "0123456789abcdef"},
		&HelloReply{Version: Version, Target: TargetInfo{Hostname: "h", UID: 1000, Home: "/home/bob"}, ScratchDir: "/home/bob/.cache/tele/s/x"},
		&ExecStart{Argv: []string{"bash", "-c", "true"}, Dir: "/", Env: []string{"A=b"}, TTY: &TTYSize{Rows: 24, Cols: 80},
			Scratch: []ScratchFile{{Area: ScratchTmp, Path: "a/b", Mode: 0o600, Data: []byte("x")}}},
		&ExecFrame{Op: ExecExit, Exit: &ExecStatus{Code: 3, WatchSeq: 7, Err: &Error{Errno: uint32(unix.ENOENT), Msg: "rg"}}},
		&FSRequest{Op: FSWrite, Path: "/a", Handle: 9, Offset: 1 << 40, Data: []byte("data"), SetAttr: &SetAttr{Valid: SetMode, Mode: 0o644}},
		&FSResponse{Errno: uint32(unix.EACCES), Entries: []DirEntry{{Name: "x", Attr: Attr{Ino: 1, Mode: unix.S_IFREG | 0o644, Mtime: -1}, Offset: 1}}},
		&WatchEvent{Seq: 1, Epoch: 2, Changes: []Change{{Dir: "/a", Name: "b", Kind: ChangeEntry}}},
		&ShimRequest{Token: []byte("t"), Name: "bash", Argv: []string{"bash", "-c", "ls"}, Dir: "/", Env: []string{"X=1"}},
		&ShimFrame{Op: ShimExit, Exit: &ShimStatus{Signal: 15, Msg: "m"}},
	}
	var buf bytes.Buffer
	for _, m := range msgs {
		if err := WriteFrame(&buf, m); err != nil {
			t.Fatalf("WriteFrame(%T): %v", m, err)
		}
	}
	for _, want := range msgs {
		got := reflect.New(reflect.TypeOf(want).Elem()).Interface()
		if err := ReadFrame(&buf, got, MaxDataFrame); err != nil {
			t.Fatalf("ReadFrame(%T): %v", want, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip %T:\n got %+v\nwant %+v", want, got, want)
		}
	}
	if err := ReadFrame(&buf, &StreamHeader{}, MaxDataFrame); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrame at clean end = %v, want io.EOF", err)
	}
}

func TestReadFrameLimits(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, &FSRequest{Op: FSWrite, Data: make([]byte, 100)}); err != nil {
		t.Fatal(err)
	}
	var tooLarge *FrameTooLargeError
	if err := ReadFrame(bytes.NewReader(buf.Bytes()), &FSRequest{}, 50); !errors.As(err, &tooLarge) {
		t.Fatalf("ReadFrame over limit = %v, want FrameTooLargeError", err)
	}
	truncated := buf.Bytes()[:buf.Len()-1]
	if err := ReadFrame(bytes.NewReader(truncated), &FSRequest{}, MaxDataFrame); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame truncated = %v, want io.ErrUnexpectedEOF", err)
	}
	if err := ReadFrame(bytes.NewReader(buf.Bytes()[:2]), &FSRequest{}, MaxDataFrame); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame truncated header = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestWriteFrameTooLarge(t *testing.T) {
	var tooLarge *FrameTooLargeError
	err := WriteFrame(io.Discard, &FSRequest{Data: make([]byte, MaxDataFrame)})
	if !errors.As(err, &tooLarge) {
		t.Fatalf("WriteFrame oversize = %v, want FrameTooLargeError", err)
	}
}

func TestUnknownFieldsIgnored(t *testing.T) {
	// A newer peer may add fields; an older peer must ignore them.
	type futureHello struct {
		Version   int    `cbor:"1,keyasint"`
		SessionID string `cbor:"3,keyasint"`
		Extra     string `cbor:"99,keyasint"`
	}
	b, err := Marshal(&futureHello{Version: 2, SessionID: "s", Extra: "new"})
	if err != nil {
		t.Fatal(err)
	}
	var h Hello
	if err := Unmarshal(b, &h); err != nil {
		t.Fatalf("Unmarshal with unknown field: %v", err)
	}
	if h.Version != 2 || h.SessionID != "s" {
		t.Fatalf("got %+v", h)
	}
}

func TestConnConcurrentSend(t *testing.T) {
	a, b := net.Pipe()
	ca, cb := NewConn(a, MaxDataFrame), NewConn(b, MaxDataFrame)
	defer ca.Close()
	defer cb.Close()

	const senders, perSender = 8, 50
	var wg sync.WaitGroup
	for i := range senders {
		wg.Go(func() {
			for j := range perSender {
				if err := ca.Send(&ExecFrame{Op: ExecStdout, PID: i*perSender + j, Data: bytes.Repeat([]byte{byte(i)}, 1000)}); err != nil {
					t.Errorf("Send: %v", err)
					return
				}
			}
		})
	}
	seen := make(map[int]bool)
	for range senders * perSender {
		var f ExecFrame
		if err := cb.Recv(&f); err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if len(f.Data) != 1000 || seen[f.PID] {
			t.Fatalf("corrupt or duplicate frame %d (len %d)", f.PID, len(f.Data))
		}
		seen[f.PID] = true
	}
	wg.Wait()
}

func TestErrorErrno(t *testing.T) {
	pe := ErrorFrom(&net.OpError{Op: "open", Err: unix.EACCES})
	if pe.Errno != uint32(unix.EACCES) {
		t.Fatalf("ErrorFrom errno = %d, want EACCES", pe.Errno)
	}
	b, err := Marshal(pe)
	if err != nil {
		t.Fatal(err)
	}
	var got Error
	if err := Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(&got, unix.EACCES) {
		t.Fatalf("errors.Is(%v, EACCES) = false", &got)
	}
	if ErrorFrom(nil) != nil {
		t.Fatal("ErrorFrom(nil) != nil")
	}
}

func TestCheckHelpers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) error
		in   string
		ok   bool
	}{
		{"path root", CheckPath, "/", true},
		{"path abs", CheckPath, "/a/b", true},
		{"path rel", CheckPath, "a/b", false},
		{"path empty", CheckPath, "", false},
		{"path dotdot", CheckPath, "/a/../b", false},
		{"path trailing slash", CheckPath, "/a/", false},
		{"path double slash", CheckPath, "//a", false},
		{"path nul", CheckPath, "/a\x00", false},
		{"name ok", CheckName, "a.b", true},
		{"name dot", CheckName, ".", false},
		{"name dotdot", CheckName, "..", false},
		{"name slash", CheckName, "a/b", false},
		{"name empty", CheckName, "", false},
		{"rel ok", CheckRelPath, "a/b", true},
		{"rel abs", CheckRelPath, "/a", false},
		{"rel dot", CheckRelPath, ".", false},
		{"rel escape", CheckRelPath, "../a", false},
		{"rel inner dotdot", CheckRelPath, "a/../b", false},
		{"rel dotdot", CheckRelPath, "..", false},
		{"sid ok", CheckSessionID, "0123456789abcdef", true},
		{"sid upper", CheckSessionID, "0123456789ABCDEF", false},
		{"sid short", CheckSessionID, "0123", false},
		{"sid space", CheckSessionID, "0123456789abcde ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.fn(tt.in); (err == nil) != tt.ok {
				t.Fatalf("%q: err = %v, want ok=%v", tt.in, err, tt.ok)
			}
		})
	}
}

func TestCheckScratch(t *testing.T) {
	ok := []ScratchFile{{Area: ScratchTmp, Path: "a"}, {Area: ScratchSessionEnv, Path: "b/c", Data: make([]byte, ScratchFileMax)}}
	if err := CheckScratch(ok); err != nil {
		t.Fatalf("CheckScratch(valid) = %v", err)
	}
	bad := [][]ScratchFile{
		{{Area: 0, Path: "a"}},
		{{Area: ScratchTmp, Path: "../a"}},
		{{Area: ScratchTmp, Path: "a", Data: make([]byte, ScratchFileMax+1)}},
	}
	for i, files := range bad {
		if err := CheckScratch(files); err == nil {
			t.Errorf("CheckScratch(bad[%d]) = nil", i)
		}
	}
	var many []ScratchFile
	for range ScratchTotalMax/ScratchFileMax + 1 {
		many = append(many, ScratchFile{Area: ScratchTmp, Path: "f", Data: make([]byte, ScratchFileMax)})
	}
	if err := CheckScratch(many); err == nil {
		t.Error("CheckScratch(over total) = nil")
	}
	for _, bad := range []ScratchFile{
		{Area: ScratchSessionEnv, Path: "d", Dir: true, Data: []byte("x")},
		{Area: ScratchSessionEnv, Path: "d", Dir: true, Deleted: true},
	} {
		if err := CheckScratch([]ScratchFile{bad}); err == nil {
			t.Errorf("CheckScratch(%+v) accepted a directory with contents or deletion", bad)
		}
	}
	if err := CheckScratch([]ScratchFile{{Area: ScratchSessionEnv, Path: "d", Dir: true}}); err != nil {
		t.Errorf("CheckScratch(directory) = %v", err)
	}
}

func FuzzReadFrame(f *testing.F) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, &FSRequest{Op: FSLookup, Path: "/a", Name: "b"})
	f.Add(buf.Bytes())
	f.Add([]byte{0, 0, 0, 1, 0xa0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(_ *testing.T, b []byte) {
		for _, v := range []any{&FSRequest{}, &ExecStart{}, &ShimRequest{}, &WatchEvent{}} {
			_ = ReadFrame(bytes.NewReader(b), v, MaxControlFrame)
		}
	})
}
