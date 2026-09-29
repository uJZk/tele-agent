package server

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/ujzk/tele-agent/internal/netrelay"
	"github.com/ujzk/tele-agent/internal/proto"
)

// forwardDialTimeout bounds connecting a forwarded stream to its port.
const forwardDialTimeout = 10 * time.Second

// serveForward connects st to a loopback TCP port on this host and relays
// bytes both ways until both directions end (docs/exec.md "端口转发"). Only
// loopback is reachable this way: the stream stands for a connection that
// Claude made to its own localhost.
func serveForward(ctx context.Context, st net.Conn) error {
	c := proto.NewConn(st, proto.MaxControlFrame)
	var open proto.ForwardOpen
	if err := c.Recv(&open); err != nil {
		return fmt.Errorf("read forward request: %w", err)
	}
	if open.Port == 0 {
		return c.Send(&proto.ForwardReply{Err: &proto.Error{Msg: "port 0"}})
	}
	dctx, cancel := context.WithTimeout(ctx, forwardDialTimeout)
	defer cancel()
	var d net.Dialer
	port := strconv.Itoa(int(open.Port))
	tc, err := d.DialContext(dctx, "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		// A server bound to ::1 only, which "localhost" may resolve to.
		tc6, err6 := d.DialContext(dctx, "tcp", net.JoinHostPort("::1", port))
		if err6 != nil {
			return c.Send(&proto.ForwardReply{Err: proto.ErrorFrom(err)})
		}
		tc = tc6
	}
	defer tc.Close()
	if err := c.Send(&proto.ForwardReply{}); err != nil {
		return err
	}
	return netrelay.Relay(st, tc)
}
