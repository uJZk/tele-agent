package telefs

import (
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

func TestMapIno(t *testing.T) {
	f := &FS{rootDev: unix.Mkdev(8, 1)}
	cases := []struct {
		name     string
		dev, ino uint64
	}{
		{"root device keeps its numbers", unix.Mkdev(8, 1), 12345},
		{"other device", unix.Mkdev(0, 42), 12345},
		{"large inode", unix.Mkdev(8, 1), 1<<63 | 7},
	}
	seen := map[uint64]string{}
	for _, tc := range cases {
		got := f.mapIno(tc.dev, tc.ino)
		if got&synthIno != 0 {
			t.Errorf("%s: %#x collides with the synthetic range", tc.name, got)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("%s and %s map to %#x", tc.name, other, got)
		}
		seen[got] = tc.name
	}
	if got := f.mapIno(unix.Mkdev(8, 1), 12345); got != 12345 {
		t.Errorf("root device inode mapped to %d", got)
	}
}

func TestRdevRoundTrip(t *testing.T) {
	for _, dev := range []uint64{unix.Mkdev(1, 3), unix.Mkdev(8, 17), unix.Mkdev(259, 70000), unix.Mkdev(4095, 1<<20-1)} {
		fr := fuseRdev(dev)
		if back := remoteRdev(fr); back != dev {
			t.Errorf("%d:%d -> %#x -> %d:%d", unix.Major(dev), unix.Minor(dev), fr, unix.Major(back), unix.Minor(back))
		}
	}
	// The kernel's new_encode_dev of 8:17 is 0x811.
	if got := fuseRdev(unix.Mkdev(8, 17)); got != 0x811 {
		t.Errorf("fuseRdev(8:17) = %#x", got)
	}
}

func TestKernelErrno(t *testing.T) {
	cases := []struct {
		remote uint32
		want   syscall.Errno
	}{
		{uint32(unix.ENOENT), unix.ENOENT},
		{uint32(unix.EHWPOISON), unix.EHWPOISON},
		{maxErrno, maxErrno},
		{512, unix.EIO}, // ERESTARTSYS
		{enotsupp, unix.EOPNOTSUPP},
		{521, unix.EIO}, // EBADHANDLE
		{528, unix.EIO}, // EJUKEBOX
		{1 << 31, unix.EIO},
		{^uint32(0), unix.EIO},
	}
	for _, tc := range cases {
		if got := kernelErrno(tc.remote); got != tc.want {
			t.Errorf("kernelErrno(%d) = %d, want %d", tc.remote, got, tc.want)
		}
	}
}

func TestCheckEvent(t *testing.T) {
	cases := []struct {
		name string
		ch   proto.Change
		ok   bool
	}{
		{"entry", proto.Change{Dir: "/a", Name: "b", Kind: proto.ChangeEntry}, true},
		{"dir itself", proto.Change{Dir: "/a", Kind: proto.ChangeEntry}, true},
		{"relative dir", proto.Change{Dir: "a", Name: "b", Kind: proto.ChangeAttr}, false},
		{"unclean dir", proto.Change{Dir: "/a/../b", Name: "b", Kind: proto.ChangeAttr}, false},
		{"dot name", proto.Change{Dir: "/a", Name: "..", Kind: proto.ChangeContent}, false},
		{"slash in name", proto.Change{Dir: "/a", Name: "b/c", Kind: proto.ChangeContent}, false},
		{"unknown kind", proto.Change{Dir: "/a", Name: "b", Kind: 9}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkEvent(&proto.WatchEvent{Seq: 1, Changes: []proto.Change{tc.ch}})
			if (err == nil) != tc.ok {
				t.Fatalf("checkEvent = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestValidResponse(t *testing.T) {
	cases := []struct {
		name string
		req  proto.FSRequest
		resp proto.FSResponse
		ok   bool
	}{
		{"lookup without attr", proto.FSRequest{Op: proto.FSLookup}, proto.FSResponse{}, false},
		{"create without handle", proto.FSRequest{Op: proto.FSCreate}, proto.FSResponse{Attr: &proto.Attr{}}, false},
		{"read longer than asked", proto.FSRequest{Op: proto.FSRead, Size: 2}, proto.FSResponse{Data: []byte("abc")}, false},
		{"read", proto.FSRequest{Op: proto.FSRead, Size: 3}, proto.FSResponse{Data: []byte("abc")}, true},
		{"readdir bad name", proto.FSRequest{Op: proto.FSReaddir}, proto.FSResponse{Entries: []proto.DirEntry{{Name: "a/b"}}}, false},
		{"readdir dots", proto.FSRequest{Op: proto.FSReaddir}, proto.FSResponse{Entries: []proto.DirEntry{{Name: "."}, {Name: ".."}, {Name: "x"}}}, true},
		{"statfs missing", proto.FSRequest{Op: proto.FSStatfs}, proto.FSResponse{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validResponse(&tc.req, &tc.resp); got != tc.ok {
				t.Fatalf("validResponse = %v, want %v", got, tc.ok)
			}
		})
	}
}

// FuzzWatchEvent feeds arbitrary server frames to the event validation,
// which guards everything RunWatch hands to the invalidation code.
func FuzzWatchEvent(f *testing.F) {
	for _, ev := range []proto.WatchEvent{
		{Seq: 1, Epoch: 2},
		{Seq: 3, Epoch: 2, Changes: []proto.Change{{Dir: "/a", Name: "b", Kind: proto.ChangeEntry}, {Dir: "/", Kind: proto.ChangeAttr}}},
	} {
		b, err := proto.Marshal(&ev)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var ev proto.WatchEvent
		if proto.Unmarshal(b, &ev) != nil {
			return
		}
		if checkEvent(&ev) != nil {
			return
		}
		for _, ch := range ev.Changes {
			if proto.CheckPath(ch.Dir) != nil || (ch.Name != "" && proto.CheckName(ch.Name) != nil) {
				t.Fatalf("accepted invalid change %+v", ch)
			}
		}
	})
}
