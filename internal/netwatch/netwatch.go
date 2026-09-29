// Package netwatch reports changes of the local network: links, addresses
// and routes, as when Wi-Fi switches or an address changes. Session main
// uses it to move its session to a new transport at once instead of
// waiting for the old one to time out (docs/transport.md "可恢复会话层").
//
// It listens on an rtnetlink socket subscribed to the link, address and
// route multicast groups, which needs no privileges.
package netwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Debounce is how long a burst of netlink messages is collected into one
// change: a switch of network brings several at once.
const Debounce = 500 * time.Millisecond

const groups = unix.RTMGRP_LINK |
	unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR |
	unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_IPV6_ROUTE

// Watch calls onChange after each burst of network changes until ctx ends.
// It returns an error only if the netlink socket cannot be set up.
func Watch(ctx context.Context, onChange func()) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netwatch: socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groups}); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("netwatch: bind: %w", err)
	}
	// Through an *os.File the socket waits in the runtime poller, and
	// closing the file ends a blocked read.
	f := os.NewFile(uintptr(fd), "netlink")
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	events := make(chan struct{}, 1)
	go func() {
		defer stop()
		defer close(events)
		buf := make([]byte, 64<<10)
		for {
			if _, err := f.Read(buf); err != nil {
				if errors.Is(err, unix.ENOBUFS) {
					// The kernel dropped messages: something changed.
					notify(events)
					continue
				}
				return
			}
			notify(events)
		}
	}()
	go debounce(events, onChange)
	return nil
}

func notify(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// debounce calls onChange once per burst of events, Debounce after the
// last one.
func debounce(events <-chan struct{}, onChange func()) {
	var timer <-chan time.Time
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
			timer = time.After(Debounce)
		case <-timer:
			timer = nil
			onChange()
		}
	}
}
