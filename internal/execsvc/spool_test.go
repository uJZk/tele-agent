package execsvc

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"time"
)

// TestSpoolOrderAndSpill writes more than the memory part holds, reading
// some in between, and gets everything back in order.
func TestSpoolOrderAndSpill(t *testing.T) {
	for _, dir := range []string{t.TempDir(), ""} {
		s := newSpool(dir)
		want := make([]byte, 3*spoolMemory/2)
		if _, err := rand.Read(want); err != nil {
			t.Fatal(err)
		}
		if dir == "" {
			want = want[:spoolMemory/2] // memory only
		}
		var got bytes.Buffer
		buf := make([]byte, 5000)
		for i := 0; i < len(want); i += 7000 {
			if _, err := s.Write(want[i:min(i+7000, len(want))]); err != nil {
				t.Fatal(err)
			}
			if i%21000 == 0 {
				n, err := s.Read(buf)
				if err != nil {
					t.Fatal(err)
				}
				got.Write(buf[:n])
			}
		}
		if dir != "" && s.file == nil {
			t.Error("spool did not spill to disk")
		}
		s.CloseWrite()
		rest, err := io.ReadAll(s)
		if err != nil {
			t.Fatal(err)
		}
		got.Write(rest)
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("dir %q: read %d bytes, not what was written (%d)", dir, got.Len(), len(want))
		}
		s.abort()
	}
}

// TestSpoolBlocksWhenFull checks the bound: a writer waits until the
// reader makes room, and abort releases it.
func TestSpoolBlocksWhenFull(t *testing.T) {
	s := newSpool("") // memory only: spoolMemory in all
	if _, err := s.Write(make([]byte, spoolMemory)); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() {
		_, err := s.Write([]byte("more"))
		wrote <- err
	}()
	select {
	case err := <-wrote:
		t.Fatalf("write to a full spool returned %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := s.Read(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	// Nearly full again: a write that does not fit waits, until abort.
	blocked := make(chan error, 1)
	go func() {
		_, err := s.Write(make([]byte, 200))
		blocked <- err
	}()
	time.Sleep(20 * time.Millisecond) //nolint:forbidigo // gives the writer time to block; not synchronization
	s.abort()
	if err := <-blocked; !errors.Is(err, errSpoolAborted) {
		t.Fatalf("blocked write after abort: %v", err)
	}
}
