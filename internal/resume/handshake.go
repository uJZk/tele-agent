package resume

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// handshakeTimeout bounds one handshake.
const handshakeTimeout = 30 * time.Second

// Redial backoff of the client.
const (
	backoffStart = 100 * time.Millisecond
	backoffMax   = 10 * time.Second
)

// DialFunc opens a transport connection to the server.
type DialFunc func(ctx context.Context) (net.Conn, error)

type redialFunc = DialFunc

// proofLabel separates this layer's MACs from any other use of the key.
const proofLabel = "tele-resume-v1"

func proof(key, nonce []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(proofLabel)) // hash writes never fail
	m.Write(nonce)
	return m.Sum(nil)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return b
}

// deadlineFrom applies ctx's deadline, and its cancellation, to tr for the
// duration of a handshake; the returned function clears both.
func deadlineFrom(ctx context.Context, tr net.Conn) (func() error, error) {
	d, ok := ctx.Deadline()
	if !ok {
		d = time.Now().Add(handshakeTimeout)
	}
	if err := tr.SetDeadline(d); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = tr.SetDeadline(time.Now()) })
	return func() error {
		stop()
		return tr.SetDeadline(time.Time{})
	}, nil
}

// Dial opens a new session over a transport from dial. The Conn redials
// with dial whenever it loses the transport.
func Dial(ctx context.Context, dial DialFunc, cfg Config) (*Conn, error) {
	key := randomBytes(proto.ResumeKeyLen)
	tr, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	reply, err := func() (*proto.ResumeReply, error) {
		endHandshake, err := deadlineFrom(ctx, tr)
		if err != nil {
			return nil, err
		}
		if err := proto.WriteFrameLimit(tr, &proto.ResumeHello{Version: proto.ResumeVersion, Key: key}, proto.MaxControlFrame); err != nil {
			return nil, fmt.Errorf("resume: send hello: %w", err)
		}
		reply, err := readReply(tr)
		if err != nil {
			return nil, err
		}
		if len(reply.Session) != proto.ResumeIDLen || reply.Recv != 0 {
			return nil, errors.New("resume: invalid reply to a new session")
		}
		return reply, endHandshake()
	}()
	if err != nil {
		_ = tr.Close()
		return nil, err
	}
	c := newConn(cfg, reply.Session, key, dial)
	if err := c.attach(tr, 0); err != nil {
		return nil, err
	}
	c.start() //nolint:contextcheck // the session outlives ctx, which only bounds dialing
	return c, nil
}

func readReply(tr net.Conn) (*proto.ResumeReply, error) {
	var reply proto.ResumeReply
	if err := proto.ReadFrame(tr, &reply, proto.MaxControlFrame); err != nil {
		return nil, fmt.Errorf("resume: read reply: %w", err)
	}
	if reply.Version != proto.ResumeVersion {
		return nil, fmt.Errorf("resume: session layer version mismatch: local %d, server %d; upgrade the older side", proto.ResumeVersion, reply.Version)
	}
	if reply.Err != nil {
		if errors.Is(reply.Err, unix.ENOENT) {
			return nil, ErrSessionLost
		}
		return nil, fmt.Errorf("resume: server refused: %w", reply.Err)
	}
	return &reply, nil
}

// reconnect redials until the transport is back, the session ends, or the
// deadline passes.
func (c *Conn) reconnect(deadline time.Time) {
	backoff := backoffStart
	for {
		c.mu.Lock()
		stop := c.tr != nil || c.termErr() != nil || c.closing
		c.mu.Unlock()
		if stop || !time.Now().Before(deadline) {
			return
		}
		ctx, cancel := context.WithDeadline(context.Background(), minTime(deadline, time.Now().Add(handshakeTimeout)))
		stopOnDone := context.AfterFunc(doneContext(c.done), cancel)
		err := c.resumeClient(ctx)
		stopOnDone()
		cancel()
		if err == nil {
			c.log.Info("resume: session resumed")
			return
		}
		if errors.Is(err, ErrSessionLost) {
			c.mu.Lock()
			c.fail(err)
			c.mu.Unlock()
			return
		}
		c.log.Debug("resume: reconnect", "err", err)
		// Jitter keeps many clients of one server apart.
		wait := backoff/2 + mrand.N(backoff/2+1) //nolint:gosec // jitter needs no secrecy
		select {
		case <-c.done:
			return
		case <-time.After(minDuration(wait, time.Until(deadline))):
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// resumeClient dials and resumes the session over the new transport.
func (c *Conn) resumeClient(ctx context.Context) error {
	tr, err := c.redial(ctx)
	if err != nil {
		return err
	}
	peerRecv, err := func() (uint64, error) {
		endHandshake, err := deadlineFrom(ctx, tr)
		if err != nil {
			return 0, err
		}
		if err := proto.WriteFrameLimit(tr, &proto.ResumeHello{Version: proto.ResumeVersion, Session: c.id}, proto.MaxControlFrame); err != nil {
			return 0, err
		}
		ch, err := readReply(tr)
		if err != nil {
			return 0, err
		}
		if len(ch.Nonce) != proto.ResumeNonceLen {
			return 0, errors.New("resume: invalid challenge")
		}
		// Stable while there is no transport: the goroutines of the
		// last one are retired.
		c.mu.Lock()
		recv := c.recv
		c.mu.Unlock()
		if err := proto.WriteFrameLimit(tr, &proto.ResumeProof{MAC: proof(c.key, ch.Nonce), Recv: recv}, proto.MaxControlFrame); err != nil {
			return 0, err
		}
		reply, err := readReply(tr)
		if err != nil {
			return 0, err
		}
		return reply.Recv, endHandshake()
	}()
	if err != nil {
		_ = tr.Close()
		return err
	}
	return c.attach(tr, peerRecv)
}

// Listener accepts sessions over the transports of a net.Listener, and
// resumes known sessions on new transports.
type Listener struct {
	ln  net.Listener
	cfg Config

	mu       sync.Mutex // guards sessions and closed
	sessions map[string]*Conn
	closed   bool

	accepted chan *Conn
	done     chan struct{} // closed by Close
	wg       sync.WaitGroup
}

// Listen serves sessions on ln, which the Listener takes over.
func Listen(ln net.Listener, cfg Config) *Listener {
	l := &Listener{
		ln:       ln,
		cfg:      cfg.withDefaults(),
		sessions: map[string]*Conn{},
		accepted: make(chan *Conn),
		done:     make(chan struct{}),
	}
	l.wg.Add(1)
	go l.acceptLoop()
	return l
}

// Accept returns the next new session.
func (l *Listener) Accept() (*Conn, error) {
	select {
	case c := <-l.accepted:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Addr returns the transport listener's address.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Close stops accepting transports and waits for the handshakes in
// progress. Accepted sessions belong to their callers and stay open.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	close(l.done)
	err := l.ln.Close()
	l.wg.Wait()
	return err
}

func (l *Listener) acceptLoop() {
	defer l.wg.Done()
	delay := 5 * time.Millisecond
	for {
		tr, err := l.ln.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A temporary failure such as EMFILE: back off.
			l.cfg.Logger.Warn("resume: accept", "err", err)
			select {
			case <-l.done:
				return
			case <-time.After(delay):
			}
			delay = min(delay*2, time.Second)
			continue
		}
		delay = 5 * time.Millisecond
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			if err := l.handshake(tr); err != nil {
				_ = tr.Close()
				l.cfg.Logger.Debug("resume: handshake", "err", err)
			}
		}()
	}
}

func (l *Listener) handshake(tr net.Conn) error {
	ctx, cancel := context.WithTimeout(doneContext(l.done), handshakeTimeout)
	defer cancel()
	endHandshake, err := deadlineFrom(ctx, tr)
	if err != nil {
		return err
	}
	var h proto.ResumeHello
	if err := proto.ReadFrame(tr, &h, proto.MaxControlFrame); err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	refuse := func(errno unix.Errno, msg string) error {
		_ = proto.WriteFrameLimit(tr, &proto.ResumeReply{Version: proto.ResumeVersion, Err: &proto.Error{Errno: uint32(errno), Msg: msg}}, proto.MaxControlFrame)
		return errors.New(msg)
	}
	if h.Version != proto.ResumeVersion {
		return refuse(unix.EPROTO, fmt.Sprintf("session layer version mismatch: server %d, client %d; upgrade the older side", proto.ResumeVersion, h.Version))
	}
	if len(h.Session) == 0 {
		return l.newSession(tr, &h, endHandshake)
	}
	c := l.lookup(h.Session)
	if c == nil {
		return refuse(unix.ENOENT, "unknown session")
	}
	nonce := randomBytes(proto.ResumeNonceLen)
	if err := proto.WriteFrameLimit(tr, &proto.ResumeReply{Version: proto.ResumeVersion, Nonce: nonce}, proto.MaxControlFrame); err != nil {
		return err
	}
	var p proto.ResumeProof
	if err := proto.ReadFrame(tr, &p, proto.MaxControlFrame); err != nil {
		return fmt.Errorf("read proof: %w", err)
	}
	if !hmac.Equal(p.MAC, proof(c.key, nonce)) {
		return refuse(unix.EACCES, "authentication failed")
	}
	return c.resumeServer(tr, p.Recv, endHandshake)
}

func (l *Listener) newSession(tr net.Conn, h *proto.ResumeHello, endHandshake func() error) error {
	if len(h.Key) != proto.ResumeKeyLen {
		return errors.New("invalid session key")
	}
	id := randomBytes(proto.ResumeIDLen)
	c := newConn(l.cfg, id, h.Key, nil)
	if err := proto.WriteFrameLimit(tr, &proto.ResumeReply{Version: proto.ResumeVersion, Session: id}, proto.MaxControlFrame); err != nil {
		return err
	}
	if err := endHandshake(); err != nil {
		return err
	}
	if err := c.attach(tr, 0); err != nil {
		return err
	}
	c.start()
	if !l.register(c) {
		_ = c.Close()
		return net.ErrClosed
	}
	select {
	case l.accepted <- c:
		return nil
	case <-l.done:
		_ = c.Close()
		return net.ErrClosed
	}
}

// register records c, forgetting sessions that ended.
func (l *Listener) register(c *Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	for k, s := range l.sessions {
		if s.Err() != nil {
			delete(l.sessions, k)
		}
	}
	l.sessions[string(c.id)] = c
	return true
}

func (l *Listener) lookup(id []byte) *Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.sessions[string(id)]
	if c == nil || c.Err() != nil {
		return nil
	}
	return c
}

// resumeServer takes tr over as the transport after a successful proof.
// Resumptions of one session run one at a time: each reports the count of
// received bytes that the new transport continues from.
func (c *Conn) resumeServer(tr net.Conn, peerRecv uint64, endHandshake func() error) error {
	c.attachMu.Lock()
	defer c.attachMu.Unlock()
	c.mu.Lock()
	if err := c.termErr(); err != nil {
		c.mu.Unlock()
		return err
	}
	// The client reconnected, so the current transport, if any, is dead.
	c.dropLocked()
	recv := c.recv
	c.mu.Unlock()
	if err := proto.WriteFrameLimit(tr, &proto.ResumeReply{Version: proto.ResumeVersion, Session: c.id, Recv: recv}, proto.MaxControlFrame); err != nil {
		return err
	}
	if err := endHandshake(); err != nil {
		return err
	}
	return c.attach(tr, peerRecv)
}

// doneContext is a context that is done when done closes.
func doneContext(done <-chan struct{}) context.Context { return doneCtx(done) }

type doneCtx <-chan struct{}

func (d doneCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (d doneCtx) Done() <-chan struct{}       { return d }
func (d doneCtx) Value(any) any               { return nil }

func (d doneCtx) Err() error {
	select {
	case <-d:
		return context.Canceled
	default:
		return nil
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func minDuration(a, b time.Duration) time.Duration {
	return max(min(a, b), 0)
}
