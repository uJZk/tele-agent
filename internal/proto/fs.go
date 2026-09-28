package proto

// FSOp identifies an FSRequest. The operations mirror FUSE operations; paths
// are absolute paths on the target host (docs/telefs.md "组成").
type FSOp uint8

// File system operations. "Path" and "Name" below refer to FSRequest fields.
const (
	// FSLookup: Name in directory Path → Attr.
	FSLookup FSOp = 1
	// FSGetattr: Path → Attr. When Handle is set, the attributes of that
	// open file or directory instead, which still works after it was
	// renamed or unlinked.
	FSGetattr FSOp = 2
	// FSSetattr: Path, or the open file or directory Handle if set, with
	// SetAttr → Attr.
	FSSetattr FSOp = 3
	// FSOpendir: Path → Handle.
	FSOpendir FSOp = 4
	// FSReaddir: Handle from Offset, at most Size entries → Entries, EOF.
	FSReaddir FSOp = 5
	// FSReleasedir: Handle.
	FSReleasedir FSOp = 6
	// FSOpen: Path with Flags → Handle.
	FSOpen FSOp = 7
	// FSCreate: Name in directory Path with Flags, Mode → Handle, Attr.
	FSCreate FSOp = 8
	// FSRead: Handle at Offset, Size bytes → Data.
	FSRead FSOp = 9
	// FSWrite: Data to Handle at Offset → Written.
	FSWrite FSOp = 10
	// FSFsync: Handle; Flags&1 requests fdatasync.
	FSFsync FSOp = 11
	// FSRelease: Handle.
	FSRelease FSOp = 12
	// FSMkdir: Name in directory Path with Mode → Attr.
	FSMkdir FSOp = 13
	// FSMknod: Name in directory Path with Mode, Rdev → Attr.
	FSMknod FSOp = 14
	// FSUnlink: Name in directory Path.
	FSUnlink FSOp = 15
	// FSRmdir: Name in directory Path.
	FSRmdir FSOp = 16
	// FSRename: Name in Path → Name2 in Path2, with renameat2 Flags.
	FSRename FSOp = 17
	// FSSymlink: Name in directory Path pointing to Target → Attr.
	FSSymlink FSOp = 18
	// FSLink: existing Path → Name2 in directory Path2 → Attr.
	FSLink FSOp = 19
	// FSReadlink: Path → Target.
	FSReadlink FSOp = 20
	// FSStatfs: Path → Statfs.
	FSStatfs FSOp = 21
	// FSAccess: Path with Mask (access(2) mode) checked as the target user.
	FSAccess FSOp = 22
	// FSGetxattr: attribute Name2 of Path, or of the open file or
	// directory Handle if set; Size 0 asks for the size only → Data or
	// Size.
	FSGetxattr FSOp = 23
	// FSListxattr: Path, or Handle if set; Size 0 asks for the size only
	// → Data or Size.
	FSListxattr FSOp = 24
	// FSSetxattr: attribute Name2 of Path, or of Handle if set, to Data
	// with Flags.
	FSSetxattr FSOp = 25
	// FSRemovexattr: attribute Name2 of Path, or of Handle if set.
	FSRemovexattr FSOp = 26
	// FSForget: the client dropped directory Path from its cache; the
	// server stops watching it. No response is expected; the server closes
	// the stream once the watch is gone.
	//
	// The server starts watching a directory (docs/telefs.md section 4)
	// when a request names it as the directory to look up, create, remove
	// or rename entries in: Path of FSLookup, FSCreate, FSMkdir, FSMknod,
	// FSUnlink, FSRmdir, FSSymlink and FSRename, Path2 of FSRename and
	// FSLink, and the directory of FSOpendir and FSReaddir.
	FSForget FSOp = 27
)

// FSRequest is the only client frame on an FS stream.
type FSRequest struct {
	Op      FSOp     `cbor:"1,keyasint"`
	Path    string   `cbor:"2,keyasint,omitempty"`
	Name    string   `cbor:"3,keyasint,omitempty"`
	Path2   string   `cbor:"4,keyasint,omitempty"`
	Name2   string   `cbor:"5,keyasint,omitempty"`
	Handle  uint64   `cbor:"6,keyasint,omitempty"`
	Offset  int64    `cbor:"7,keyasint,omitempty"`
	Size    uint32   `cbor:"8,keyasint,omitempty"`
	Data    []byte   `cbor:"9,keyasint,omitempty"`
	Flags   uint32   `cbor:"10,keyasint,omitempty"`
	Mode    uint32   `cbor:"11,keyasint,omitempty"`
	Rdev    uint64   `cbor:"12,keyasint,omitempty"`
	SetAttr *SetAttr `cbor:"13,keyasint,omitempty"`
	Target  string   `cbor:"14,keyasint,omitempty"`
	Mask    uint32   `cbor:"15,keyasint,omitempty"`
	// Node, on a request that acts on the object at Path itself
	// (FSGetattr, FSSetattr, FSAccess, FSReadlink and the xattr
	// requests), is the remote identity the client means. If Path names
	// another object now, for example after a rename replaced it, the
	// request fails with ESTALE instead of acting on that object; for a
	// path-based system call the kernel then repeats the lookup and the
	// call once (docs/telefs.md section 3).
	Node *NodeID `cbor:"16,keyasint,omitempty"`
}

// NodeID identifies an object on the target host.
type NodeID struct {
	Dev uint64 `cbor:"1,keyasint"`
	Ino uint64 `cbor:"2,keyasint"`
}

// FSResponse is the only server frame on an FS stream. Errno is the remote
// errno, returned unchanged to the kernel whenever the kernel can take it
// (docs/coding-standards.md "错误处理").
type FSResponse struct {
	Errno   uint32     `cbor:"1,keyasint,omitempty"`
	Attr    *Attr      `cbor:"2,keyasint,omitempty"`
	Entries []DirEntry `cbor:"3,keyasint,omitempty"`
	EOF     bool       `cbor:"4,keyasint,omitempty"`
	Data    []byte     `cbor:"5,keyasint,omitempty"`
	Handle  uint64     `cbor:"6,keyasint,omitempty"`
	Written uint32     `cbor:"7,keyasint,omitempty"`
	Size    uint32     `cbor:"8,keyasint,omitempty"`
	Statfs  *Statfs    `cbor:"9,keyasint,omitempty"`
	Target  string     `cbor:"10,keyasint,omitempty"`
	// Unwatched reports that the server could not watch the directory the
	// request named (for example EACCES or the inotify watch limit), so no
	// change to it will be pushed and the client must cache what this
	// response returns only briefly (docs/telefs.md sections 3 and 4).
	Unwatched bool `cbor:"11,keyasint,omitempty"`
}

// Attr is the result of lstat on the target host. Owners are the raw remote
// IDs; the client decides how to present them.
type Attr struct {
	Dev     uint64 `cbor:"1,keyasint"`
	Ino     uint64 `cbor:"2,keyasint"`
	Mode    uint32 `cbor:"3,keyasint"`
	Nlink   uint32 `cbor:"4,keyasint,omitempty"`
	UID     uint32 `cbor:"5,keyasint,omitempty"`
	GID     uint32 `cbor:"6,keyasint,omitempty"`
	Rdev    uint64 `cbor:"7,keyasint,omitempty"`
	Size    int64  `cbor:"8,keyasint,omitempty"`
	Blocks  int64  `cbor:"9,keyasint,omitempty"`
	Blksize int32  `cbor:"10,keyasint,omitempty"`
	// Times are nanoseconds since the Unix epoch.
	Atime int64 `cbor:"11,keyasint,omitempty"`
	Mtime int64 `cbor:"12,keyasint,omitempty"`
	Ctime int64 `cbor:"13,keyasint,omitempty"`
}

// SetAttr bits select which SetAttr fields apply.
const (
	SetMode     = 1 << 0
	SetUID      = 1 << 1
	SetGID      = 1 << 2
	SetSize     = 1 << 3
	SetAtime    = 1 << 4
	SetMtime    = 1 << 5
	SetAtimeNow = 1 << 6
	SetMtimeNow = 1 << 7
)

// SetAttr describes a setattr request.
type SetAttr struct {
	Valid uint32 `cbor:"1,keyasint"`
	Mode  uint32 `cbor:"2,keyasint,omitempty"`
	UID   uint32 `cbor:"3,keyasint,omitempty"`
	GID   uint32 `cbor:"4,keyasint,omitempty"`
	Size  int64  `cbor:"5,keyasint,omitempty"`
	// Times are nanoseconds since the Unix epoch.
	Atime int64 `cbor:"6,keyasint,omitempty"`
	Mtime int64 `cbor:"7,keyasint,omitempty"`
}

// DirEntry is one directory entry with its attributes (readdirplus).
// Offset is the value to pass in the next FSReaddir to continue after it;
// it is the directory cookie of the target host and never 0, which means
// "from the start". Listings include "." and "..".
type DirEntry struct {
	Name   string `cbor:"1,keyasint"`
	Attr   Attr   `cbor:"2,keyasint"`
	Offset int64  `cbor:"3,keyasint"`
	// TypeOnly reports that the entry could be listed but not examined
	// (for example in a directory with read but no search permission):
	// Attr carries only Dev, Ino and the file type of the directory
	// entry, which must not be cached as the entry's attributes.
	TypeOnly bool `cbor:"4,keyasint,omitempty"`
}

// Statfs is the result of statfs on the target host.
type Statfs struct {
	Blocks  uint64 `cbor:"1,keyasint"`
	Bfree   uint64 `cbor:"2,keyasint"`
	Bavail  uint64 `cbor:"3,keyasint"`
	Files   uint64 `cbor:"4,keyasint"`
	Ffree   uint64 `cbor:"5,keyasint"`
	Bsize   uint32 `cbor:"6,keyasint"`
	Namelen uint32 `cbor:"7,keyasint"`
	Frsize  uint32 `cbor:"8,keyasint"`
}

// MaxIO bounds the Size of one FSRead and the Data of one FSWrite.
const MaxIO = 1 << 20
