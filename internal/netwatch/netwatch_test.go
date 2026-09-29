package netwatch

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/testutil/privtest"
)

func TestDebounce(t *testing.T) {
	events := make(chan struct{})
	var calls atomic.Int64
	done := make(chan struct{})
	go func() {
		debounce(events, func() { calls.Add(1) })
		close(done)
	}()
	// A burst is one change.
	for range 5 {
		events <- struct{}{}
		time.Sleep(Debounce / 10)
	}
	time.Sleep(2 * Debounce)
	if n := calls.Load(); n != 1 {
		t.Fatalf("a burst gave %d calls, want 1", n)
	}
	// A later event is another change.
	events <- struct{}{}
	time.Sleep(2 * Debounce)
	if n := calls.Load(); n != 2 {
		t.Fatalf("a second burst gave %d calls in all, want 2", n)
	}
	close(events)
	<-done
}

// TestWatchSeesLinkChange brings up the loopback interface of a fresh
// network namespace and expects a change. The namespace belongs to one
// locked OS thread, which exits with the test goroutine; the netlink socket
// is created on that thread, so it watches that namespace.
func TestWatchSeesLinkChange(t *testing.T) {
	privtest.RequireRoot(t)
	changed := make(chan struct{}, 1)
	errc := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		runtime.LockOSThread() // never unlocked: the thread dies with the goroutine
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			errc <- err
			return
		}
		if err := Watch(ctx, func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		}); err != nil {
			errc <- err
			return
		}
		errc <- setLoopbackUp()
	}()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("no change reported after bringing lo up")
	}
}

// setLoopbackUp sets IFF_UP on lo in the calling thread's namespace.
func setLoopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var req [unix.IFNAMSIZ + 24]byte
	copy(req[:], "lo")
	flags := (*uint16)(unsafe.Pointer(&req[unix.IFNAMSIZ]))
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&req))); e != 0 {
		return e
	}
	*flags |= unix.IFF_UP
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&req))); e != 0 {
		return e
	}
	return nil
}
