// Package resume is the resumable session layer (docs/transport.md
// "可恢复会话层"): a byte stream between session main and tele server that
// survives the loss of the transport connection it runs over. The
// multiplexer (internal/mux) runs on top of it, so no stream of a session
// notices a reconnect.
//
// Each side keeps the bytes it sent until the peer acknowledges them, and
// counts the bytes it received. A new transport connection starts with a
// handshake (proto.ResumeHello) in which each side reports its count; each
// then sends the peer's missing bytes again, so every byte arrives exactly
// once and in order. Resuming needs the session key, not only the ID.
//
// Liveness belongs to this layer: both sides ping at a fixed interval and
// drop a transport that stays silent for three intervals, which also
// catches half-open connections. The client redials with backoff; the
// server waits. A session that has no transport for longer than its lease
// ends on both sides with ErrExpired. An orderly Close sends a
// SessionFin after the last byte, so that the peer ends at once instead of
// waiting out the lease; the peer answers with its own SessionFin, and only
// then is the transport closed: closing it earlier could discard bytes
// still in flight (a TCP socket closed with unread data resets the
// connection).
package resume

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/ujzk/tele-agent/internal/proto"
)

// Errors of a Conn.
var (
	// ErrExpired: the session had no transport for longer than its lease.
	ErrExpired = errors.New("resume: session lease expired")
	// ErrClosed: the Conn was closed locally.
	ErrClosed = errors.New("resume: session closed")
	// ErrPeerClosed: the peer ended the session; writes fail with it
	// (reads return io.EOF once the received bytes were read).
	ErrPeerClosed = errors.New("resume: session ended by the peer")
	// ErrSessionLost: the server no longer knows the session, typically
	// because it restarted (docs/transport.md "会话层覆盖不到的情况").
	ErrSessionLost = errors.New("resume: the server no longer has the session")
)

// Defaults of Config.
const (
	// DefaultLease is how long a session outlives its transport. The
	// multiplexer's write timeout matches it (internal/mux).
	DefaultLease = 30 * time.Minute
	// DefaultHeartbeat is the ping interval; three silent intervals drop
	// the transport.
	DefaultHeartbeat = 5 * time.Second
	// DefaultMaxBuffer bounds the bytes sent but not acknowledged; Write
	// blocks beyond it. The multiplexer's stream windows keep the actual
	// amount far lower.
	DefaultMaxBuffer = 64 << 20
)

// Internal limits.
const (
	// maxRecvBuffer bounds the received bytes not yet Read; beyond it the
	// transport is not read, which pushes back on the peer. The
	// multiplexer reads continuously, so it is never reached in practice.
	maxRecvBuffer = 16 << 20
	// ackEvery is how many received bytes trigger an acknowledgement
	// before the next heartbeat would.
	ackEvery = 256 << 10
	// closeTimeout bounds how long Close waits for the last bytes and the
	// SessionFin to leave.
	closeTimeout = 5 * time.Second
	// silentBeats is how many heartbeat intervals without a frame drop
	// the transport.
	silentBeats = 3
)

// Config configures both ends.
type Config struct {
	Lease     time.Duration
	Heartbeat time.Duration
	MaxBuffer int
	// Logger receives diagnostics; nil discards them.
	Logger *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.Lease <= 0 {
		c.Lease = DefaultLease
	}
	if c.Heartbeat <= 0 {
		c.Heartbeat = DefaultHeartbeat
	}
	if c.MaxBuffer <= 0 {
		c.MaxBuffer = DefaultMaxBuffer
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	return c
}

// Conn is one end of a resumable session. Read, Write and Close may be
// called concurrently; it implements io.ReadWriteCloser.
type Conn struct {
	cfg Config
	log *slog.Logger
	id  []byte
	key []byte
	// redial reconnects on the client; nil on the server.
	redial redialFunc
	// attachMu serializes resumptions on the server (resumeServer).
	attachMu sync.Mutex

	mu   sync.Mutex // guards the fields below
	cond *sync.Cond // broadcast on every change of them

	// buf holds the bytes written but not acknowledged; buf[0] is the
	// session byte number acked. sent is the next byte to transmit on the
	// current transport.
	buf   []byte
	acked uint64
	sent  uint64
	// rbuf holds the bytes received but not read; recv counts every byte
	// received, ackSent the count last acknowledged.
	rbuf    []byte
	recv    uint64
	ackSent uint64
	ackDue  bool
	pingDue bool
	pongDue bool

	// tr is the current transport, nil while there is none. gen changes
	// whenever tr does, retiring the goroutines of older transports.
	tr        net.Conn
	gen       uint64
	lastHeard time.Time
	downSince time.Time

	closing    bool  // Close was called: send the SessionFin
	finSent    bool  // the SessionFin was handed to the writer
	finWritten bool  // and the transport took it
	peerFin    bool  // the peer's SessionFin arrived
	err        error // terminal: ErrExpired, ErrClosed, a protocol error

	onExpire func() // run before the Conn fails with ErrExpired

	done chan struct{} // closed once terminal (err set or peerFin)
	wg   sync.WaitGroup
}

func newConn(cfg Config, id, key []byte, redial redialFunc) *Conn {
	cfg = cfg.withDefaults()
	c := &Conn{
		cfg:       cfg,
		log:       cfg.Logger,
		id:        id,
		key:       key,
		redial:    redial,
		downSince: time.Now(),
		done:      make(chan struct{}),
	}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// start runs the goroutines that outlive transports.
func (c *Conn) start() {
	c.wg.Add(2)
	go c.heartbeat()
	go c.supervise()
}

// ID returns the session ID.
func (c *Conn) ID() []byte { return c.id }

// Done is closed when the session ended, for whatever reason (Err).
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns why the session ended, or nil while it runs. A session the
// peer ended returns ErrPeerClosed.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.termErr()
}

// termErr returns the terminal error; the caller holds mu.
func (c *Conn) termErr() error {
	switch {
	case c.err != nil:
		return c.err
	case c.peerFin:
		return ErrPeerClosed
	}
	return nil
}

// OnExpire sets a function that runs when the lease expires, before the
// Conn fails: the server stops the session's commands gracefully while its
// streams are still intact (docs/transport.md "断线语义").
func (c *Conn) OnExpire(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onExpire = f
}

// fail makes the Conn terminal with err; the caller holds mu.
func (c *Conn) fail(err error) {
	if c.err != nil {
		return
	}
	if !errors.Is(err, ErrClosed) {
		c.log.Warn("resume: session failed", "err", err)
	}
	c.err = err
	c.dropLocked()
	c.markDone()
	c.cond.Broadcast()
}

func (c *Conn) markDone() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// Write appends p to the session. It blocks while MaxBuffer bytes wait for
// acknowledgement, which is the case while there is no transport.
func (c *Conn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if err := c.writeErr(); err != nil {
			return 0, err
		}
		if len(c.buf) < c.cfg.MaxBuffer {
			break
		}
		c.cond.Wait()
	}
	c.buf = append(c.buf, p...)
	c.cond.Broadcast()
	return len(p), nil
}

func (c *Conn) writeErr() error {
	switch {
	case c.err != nil:
		return c.err
	case c.closing:
		return ErrClosed
	case c.peerFin:
		return ErrPeerClosed
	}
	return nil
}

// Read reads the next bytes of the session. It returns io.EOF once the
// peer ended the session and everything it sent was read.
func (c *Conn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.rbuf) == 0 {
		switch {
		case c.err != nil:
			return 0, c.err
		case c.peerFin:
			return 0, io.EOF
		}
		c.cond.Wait()
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	if len(c.rbuf) == 0 {
		c.rbuf = nil // let the buffer go
	}
	c.cond.Broadcast()
	return n, nil
}

// Close ends the session: what was written is sent first, with a
// SessionFin behind it, and the peer's SessionFin is awaited, as long as a
// transport carries them within a few seconds. It waits for the Conn's
// goroutines.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.err == nil {
		c.closing = true
		c.cond.Broadcast()
		timer := time.AfterFunc(closeTimeout, func() {
			c.mu.Lock()
			c.fail(ErrClosed)
			c.mu.Unlock()
		})
		// Both SessionFins must have passed: ours, and the peer's (or
		// ours answering it).
		for c.err == nil && c.tr != nil && (!c.peerFin || !c.finWritten) {
			c.cond.Wait()
		}
		timer.Stop()
	}
	c.fail(ErrClosed)
	c.mu.Unlock()
	c.wg.Wait()
	return nil
}

// dropLocked closes the current transport; the caller holds mu.
func (c *Conn) dropLocked() {
	if c.tr == nil {
		return
	}
	_ = c.tr.Close() // its goroutines see the error and end
	c.tr = nil
	c.gen++
	c.downSince = time.Now()
	c.cond.Broadcast()
}

// drop closes transport generation gen after it failed with err.
func (c *Conn) drop(gen uint64, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen || c.tr == nil {
		return
	}
	if c.termErr() == nil {
		c.log.Info("resume: transport lost", "err", err)
	}
	c.dropLocked()
}

// attach makes tr the transport after a handshake in which the peer said
// it received peerRecv bytes. It fails if the peer's count cannot be
// right, which would lose or repeat bytes.
func (c *Conn) attach(tr net.Conn, peerRecv uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.termErr(); err != nil {
		_ = tr.Close()
		return err
	}
	if peerRecv < c.acked || peerRecv > c.acked+uint64(len(c.buf)) {
		err := fmt.Errorf("resume: peer reports %d bytes received, but %d to %d are possible", peerRecv, c.acked, c.acked+uint64(len(c.buf)))
		_ = tr.Close()
		c.fail(err)
		return err
	}
	c.dropLocked()
	c.buf = c.buf[peerRecv-c.acked:]
	c.acked, c.sent = peerRecv, peerRecv
	// A SessionFin sent on the lost transport goes again, behind the
	// bytes sent again.
	c.finSent, c.finWritten = false, false
	// The handshake told the peer what arrived here.
	c.ackSent = c.recv
	c.tr = tr
	c.gen++
	c.lastHeard = time.Now()
	gen := c.gen
	c.wg.Add(2)
	go c.reader(tr, gen)
	go c.writer(tr, gen)
	c.cond.Broadcast()
	return nil
}

// reader receives the frames of transport generation gen.
func (c *Conn) reader(tr net.Conn, gen uint64) {
	defer c.wg.Done()
	for {
		var f proto.SessionFrame
		if err := proto.ReadFrame(tr, &f, proto.MaxSessionFrame); err != nil {
			c.drop(gen, err)
			return
		}
		if err := f.Check(); err != nil {
			c.mu.Lock()
			c.fail(err)
			c.mu.Unlock()
			return
		}
		if !c.receive(gen, &f) {
			return
		}
	}
}

// receive applies frame f of generation gen and reports whether to go on.
func (c *Conn) receive(gen uint64, f *proto.SessionFrame) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen || c.err != nil {
		return false
	}
	c.lastHeard = time.Now()
	switch f.Kind {
	case proto.SessionData:
		for len(c.rbuf) >= maxRecvBuffer && gen == c.gen && c.err == nil {
			c.cond.Wait()
		}
		if gen != c.gen || c.err != nil {
			return false
		}
		c.rbuf = append(c.rbuf, f.Data...)
		c.recv += uint64(len(f.Data))
		if c.recv-c.ackSent >= ackEvery {
			c.ackDue = true
		}
	case proto.SessionAck:
		if f.Ack > c.sent {
			c.fail(fmt.Errorf("resume: peer acknowledges byte %d, only %d were sent", f.Ack, c.sent))
			return false
		}
		if f.Ack > c.acked {
			c.buf = c.buf[f.Ack-c.acked:]
			c.acked = f.Ack
		}
	case proto.SessionPing:
		c.pongDue = true
	case proto.SessionPong:
	case proto.SessionFin:
		c.peerFin = true
		c.markDone()
	}
	c.cond.Broadcast()
	return true
}

// writer sends the frames of transport generation gen.
func (c *Conn) writer(tr net.Conn, gen uint64) {
	defer c.wg.Done()
	for {
		frames, fin, ok := c.nextFrames(gen)
		if !ok {
			return
		}
		var out []byte
		for i := range frames {
			b, err := encode(&frames[i])
			if err != nil {
				c.mu.Lock()
				c.fail(err)
				c.mu.Unlock()
				return
			}
			out = append(out, b...)
		}
		if _, err := tr.Write(out); err != nil {
			c.drop(gen, err)
			return
		}
		if fin {
			c.mu.Lock()
			if gen == c.gen {
				c.finWritten = true
				c.cond.Broadcast()
			}
			c.mu.Unlock()
		}
	}
}

// nextFrames waits until there is something to send on generation gen and
// returns it: at most one data frame behind the control frames due. ok is
// false once gen is retired.
//
// The frames count as sent from here on: the peer may acknowledge them
// before the transport write returns. Should the write fail, the next
// transport starts over from the count the peer reports.
func (c *Conn) nextFrames(gen uint64) (frames []proto.SessionFrame, fin, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.cond.Broadcast()
	for {
		if gen != c.gen || c.err != nil || c.finSent {
			return nil, false, false
		}
		if c.pongDue {
			frames = append(frames, proto.SessionFrame{Kind: proto.SessionPong})
			c.pongDue = false
		}
		if c.pingDue {
			frames = append(frames, proto.SessionFrame{Kind: proto.SessionPing})
			c.pingDue = false
		}
		if c.ackDue && c.recv > c.ackSent {
			frames = append(frames, proto.SessionFrame{Kind: proto.SessionAck, Ack: c.recv})
			c.ackSent = c.recv
		}
		c.ackDue = false
		if c.peerFin {
			// The peer is gone: answer its SessionFin, send nothing more.
			frames = append(frames, proto.SessionFrame{Kind: proto.SessionFin})
			c.finSent, fin = true, true
		} else if off := c.sent - c.acked; off < uint64(len(c.buf)) {
			chunk := c.buf[off:min(uint64(len(c.buf)), off+proto.MaxSessionData)]
			// Copied: buf moves when acknowledgements trim it.
			frames = append(frames, proto.SessionFrame{Kind: proto.SessionData, Data: append([]byte(nil), chunk...)})
			c.sent += uint64(len(chunk))
		} else if c.closing {
			frames = append(frames, proto.SessionFrame{Kind: proto.SessionFin})
			c.finSent, fin = true, true
		}
		if len(frames) > 0 {
			return frames, fin, true
		}
		c.cond.Wait()
	}
}

func encode(f *proto.SessionFrame) ([]byte, error) {
	var b bytesWriter
	if err := proto.WriteFrameLimit(&b, f, proto.MaxSessionFrame); err != nil {
		return nil, err
	}
	return b, nil
}

// bytesWriter collects one frame.
type bytesWriter []byte

func (b *bytesWriter) Write(p []byte) (int, error) {
	*b = append(*b, p...)
	return len(p), nil
}

// heartbeat pings the peer, acknowledges what arrived, and drops a
// transport that stays silent.
func (c *Conn) heartbeat() {
	defer c.wg.Done()
	t := time.NewTicker(c.cfg.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
		}
		c.mu.Lock()
		if c.tr != nil {
			if time.Since(c.lastHeard) > silentBeats*c.cfg.Heartbeat {
				c.log.Info("resume: transport silent; dropping it")
				c.dropLocked()
			} else {
				c.pingDue = true
				c.ackDue = true
			}
		}
		c.cond.Broadcast()
		c.mu.Unlock()
	}
}

// supervise restores the transport (client) or waits for the client to
// (server), and ends the session when the lease runs out.
func (c *Conn) supervise() {
	defer c.wg.Done()
	for {
		c.mu.Lock()
		for c.tr != nil && c.termErr() == nil {
			c.cond.Wait()
		}
		if c.termErr() != nil || c.closing {
			c.mu.Unlock()
			return
		}
		deadline := c.downSince.Add(c.cfg.Lease)
		c.mu.Unlock()

		if c.redial != nil {
			c.reconnect(deadline)
		} else {
			c.awaitResume(deadline)
		}
		c.mu.Lock()
		expired := c.tr == nil && c.termErr() == nil && !time.Now().Before(deadline)
		hook := c.onExpire
		c.mu.Unlock()
		if expired {
			c.expire(hook)
		}
	}
}

// expire ends the session because its lease ran out, after running the
// expiry hook while the streams are still intact.
func (c *Conn) expire(hook func()) {
	c.log.Warn("resume: session lease expired")
	if hook != nil {
		hook()
	}
	c.mu.Lock()
	c.fail(ErrExpired)
	c.mu.Unlock()
}

// awaitResume waits until a transport is attached, the session ends, or
// the deadline passes.
func (c *Conn) awaitResume(deadline time.Time) {
	timer := time.AfterFunc(time.Until(deadline), func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer timer.Stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.tr == nil && c.termErr() == nil && !c.closing && time.Now().Before(deadline) {
		c.cond.Wait()
	}
}
