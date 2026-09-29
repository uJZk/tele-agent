package resume

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ujzk/tele-agent/internal/testutil/faultnet"
)

// migratePair is pairOf with a dial that tests can make fail and count.
type migratePair struct {
	client, server *Conn
	px             *faultnet.Proxy
	failDial       atomic.Bool
	dials          atomic.Int64
}

func newMigratePair(t *testing.T, cfg Config) *migratePair {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", filepath.Join(t.TempDir(), "server.sock"))
	if err != nil {
		t.Fatal(err)
	}
	l := Listen(ln, cfg)
	t.Cleanup(func() { _ = l.Close() })
	p := &migratePair{px: faultnet.New(t, ln.Addr().String())}
	dial := func(ctx context.Context) (net.Conn, error) {
		p.dials.Add(1)
		if p.failDial.Load() {
			return nil, errors.New("dial disabled by the test")
		}
		var d net.Dialer
		return d.DialContext(ctx, "unix", p.px.Addr())
	}
	if p.client, err = Dial(t.Context(), dial, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.client.Close() })
	if p.server, err = l.Accept(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.server.Close() })
	return p
}

// TestExactlyOnceAcrossMigrations moves the session to new transports
// again and again while data flows both ways: every byte arrives once, in
// order, and no migration costs the session.
func TestExactlyOnceAcrossMigrations(t *testing.T) {
	p := newMigratePair(t, fastConfig)
	up, down := randomData(t, 4<<20), randomData(t, 4<<20)
	gotUp := transfer(t, p.client, p.server, up)
	gotDown := transfer(t, p.server, p.client, down)
	stop := make(chan struct{})
	var migrations atomic.Int64
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				if err := p.client.Migrate(t.Context()); err == nil {
					migrations.Add(1)
				}
			}
		}
	})
	u := wait(t, gotUp, "the client's data")
	d := wait(t, gotDown, "the server's data")
	close(stop)
	wg.Wait()
	if !bytes.Equal(u, up) || !bytes.Equal(d, down) {
		t.Fatal("bytes lost, repeated or reordered across migrations")
	}
	if migrations.Load() == 0 {
		t.Fatal("no migration happened during the transfer")
	}
	if err := p.client.Err(); err != nil {
		t.Fatalf("session ended: %v", err)
	}
}

// TestMigrateDialFailureKeepsTransport checks make-before-break: when the
// new transport cannot be dialed, the old one is kept.
func TestMigrateDialFailureKeepsTransport(t *testing.T) {
	p := newMigratePair(t, fastConfig)
	p.failDial.Store(true)
	if err := p.client.Migrate(t.Context()); err == nil {
		t.Fatal("Migrate succeeded without a transport to dial")
	}
	// The old transport still carries data, with no redial needed.
	before := p.dials.Load()
	msg := []byte("still here")
	got := transfer(t, p.client, p.server, msg)
	if g := wait(t, got, "data over the old transport"); !bytes.Equal(g, msg) {
		t.Fatalf("got %q", g)
	}
	if p.dials.Load() != before {
		t.Fatal("the session redialed although its transport was fine")
	}
}

// TestKickShortensBackoff lets the reconnect backoff grow, then kicks the
// client: it reconnects at once instead of after the backoff.
func TestKickShortensBackoff(t *testing.T) {
	p := newMigratePair(t, Config{Lease: time.Minute, Heartbeat: 50 * time.Millisecond})
	p.failDial.Store(true)
	p.px.Cut()
	// Several failed attempts grow the backoff towards backoffMax.
	deadline := time.Now().Add(10 * time.Second)
	for p.dials.Load() < 7 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	p.failDial.Store(false)
	start := time.Now()
	p.client.Kick()
	msg := []byte("after the kick")
	got := transfer(t, p.client, p.server, msg)
	if g := wait(t, got, "data after the kick"); !bytes.Equal(g, msg) {
		t.Fatalf("got %q", g)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("reconnected %v after the kick; the backoff was not cut short", d)
	}
}

// TestSilentTransportMigrates holes the transport: the client tries a new
// one after suspectBeats, before dropping the old one after silentBeats.
func TestSilentTransportMigrates(t *testing.T) {
	var logs syncBuffer
	cfg := fastConfig
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	p := newMigratePair(t, cfg)
	p.px.Hole()
	msg := []byte("after the hole")
	got := transfer(t, p.client, p.server, msg)
	if g := wait(t, got, "data after the hole"); !bytes.Equal(g, msg) {
		t.Fatalf("got %q", g)
	}
	if !strings.Contains(logs.String(), "trying a new one") {
		t.Fatalf("the client did not try a new transport early:\n%s", logs.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
