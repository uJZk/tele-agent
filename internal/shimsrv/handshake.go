package shimsrv

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// stdioFDs is the number of fds a shim passes: its 0, 1 and 2.
const stdioFDs = 3

// maxRecvFDs sizes the control buffer. It is larger than stdioFDs so that
// a peer sending a few extra fds is detected and those fds are closed; the
// kernel discards (and flags with MSG_CTRUNC) any that do not fit.
const maxRecvFDs = 16

// handshake authenticates a new connection and reads its request: the
// peer's uid, the marker byte carrying the stdio fds, the ShimRequest and
// its token, in protocol order. On error every received fd is closed.
func (s *Server) handshake(ctx context.Context, conn *net.UnixConn) (*Request, error) {
	cred, err := peerCred(conn)
	if err != nil {
		return nil, err
	}
	if err := checkPeer(cred, s.UID); err != nil {
		return nil, err
	}

	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) }) // wakes blocked reads
	defer stop()

	fds, err := recvStdio(conn)
	if err != nil {
		return nil, err
	}
	var sr proto.ShimRequest
	if err := proto.ReadFrame(conn, &sr, proto.MaxControlFrame); err != nil {
		closeFDs(fds)
		return nil, fmt.Errorf("read shim request: %w", err)
	}
	// An empty server token never matches: ConstantTimeCompare of two empty
	// slices reports equality.
	if len(s.Token) == 0 || subtle.ConstantTimeCompare(sr.Token, s.Token) != 1 {
		closeFDs(fds)
		return nil, errTokenRejected
	}
	if sr.Name == "" || len(sr.Argv) == 0 {
		closeFDs(fds)
		return nil, errors.New("shim request without name or argv")
	}
	if !stop() {
		closeFDs(fds)
		return nil, ctx.Err()
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		closeFDs(fds)
		return nil, err
	}
	return &Request{
		Name:    sr.Name,
		Argv:    sr.Argv,
		Dir:     sr.Dir,
		Env:     sr.Env,
		Stdin:   os.NewFile(uintptr(fds[0]), "shim-stdin"),
		Stdout:  os.NewFile(uintptr(fds[1]), "shim-stdout"),
		Stderr:  os.NewFile(uintptr(fds[2]), "shim-stderr"),
		PeerPID: int(cred.Pid),
	}, nil
}

func peerCred(conn *net.UnixConn) (*unix.Ucred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var (
		cred    *unix.Ucred
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	if credErr != nil {
		return nil, fmt.Errorf("SO_PEERCRED: %w", credErr)
	}
	return cred, nil
}

// checkPeer accepts only processes of the session's user.
func checkPeer(cred *unix.Ucred, uid int) error {
	if int64(cred.Uid) != int64(uid) {
		return fmt.Errorf("peer pid %d has uid %d, want %d", cred.Pid, cred.Uid, uid)
	}
	return nil
}

// recvStdio reads the marker byte and exactly the three fds attached to it.
// The fds arrive close-on-exec (net requests MSG_CMSG_CLOEXEC), so they
// never leak into unrelated children of session main.
func recvStdio(conn *net.UnixConn) ([]int, error) {
	var b [1]byte
	oob := make([]byte, unix.CmsgSpace(maxRecvFDs*4))
	n, oobn, flags, _, err := conn.ReadMsgUnix(b[:], oob)
	fds, parseErr := parseRights(oob[:oobn])
	switch {
	case err != nil:
		err = fmt.Errorf("read stdio fds: %w", err)
	case parseErr != nil:
		err = parseErr
	case n != 1:
		err = errors.New("read stdio fds: no marker byte")
	case flags&unix.MSG_CTRUNC != 0:
		err = errors.New("read stdio fds: too many fds")
	case len(fds) != stdioFDs:
		err = fmt.Errorf("read stdio fds: got %d fds, want %d", len(fds), stdioFDs)
	}
	if err != nil {
		closeFDs(fds)
		return nil, err
	}
	return fds, nil
}

// parseRights returns every fd in the control messages. On error it still
// returns the fds found, which the caller must close.
func parseRights(oob []byte) ([]int, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, fmt.Errorf("parse control message: %w", err)
	}
	var fds []int
	for i := range msgs {
		m := &msgs[i]
		if m.Header.Level != unix.SOL_SOCKET || m.Header.Type != unix.SCM_RIGHTS {
			err = fmt.Errorf("unexpected control message %d/%d", m.Header.Level, m.Header.Type)
			continue
		}
		got, perr := unix.ParseUnixRights(m)
		if perr != nil {
			err = fmt.Errorf("parse SCM_RIGHTS: %w", perr)
			continue
		}
		fds = append(fds, got...)
	}
	return fds, err
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = unix.Close(fd) // received, never used: nothing to report
	}
}
