package proto

import "fmt"

// Scratch limits (docs/exec.md section 5).
const (
	// ScratchFileMax bounds one scratch file carried in either direction.
	ScratchFileMax = 1 << 20
	// ScratchTotalMax bounds all scratch files carried in one message.
	ScratchTotalMax = 8 << 20
)

// ExecStart is the first frame a client sends on an exec stream, after
// the StreamHeader.
type ExecStart struct {
	// Argv is the command to run. An Argv[0] without a slash is looked up
	// in TargetInfo.LoginPath.
	Argv []string `cbor:"1,keyasint"`
	// Dir is the working directory on the target host.
	Dir string `cbor:"2,keyasint"`
	// Env holds KEY=VALUE entries applied over the server's base
	// environment for this session (docs/exec.md section 3).
	Env []string `cbor:"3,keyasint,omitempty"`
	// TTY requests a pseudo-terminal of this size; nil means pipes, with
	// stdout and stderr kept separate.
	TTY *TTYSize `cbor:"4,keyasint,omitempty"`
	// Scratch is written into the session's scratch areas before the
	// command starts.
	Scratch []ScratchFile `cbor:"5,keyasint,omitempty"`
}

// TTYSize is a terminal size in character cells.
type TTYSize struct {
	Rows uint16 `cbor:"1,keyasint"`
	Cols uint16 `cbor:"2,keyasint"`
}

// ExecOp identifies an ExecFrame.
type ExecOp uint8

// Exec frame operations. Values below 16 flow client → server, the rest
// server → client.
const (
	// ExecStdin carries Data for the command's stdin.
	ExecStdin ExecOp = 1
	// ExecStdinEOF closes the command's stdin.
	ExecStdinEOF ExecOp = 2
	// ExecSignal delivers Signal to the command's process group.
	ExecSignal ExecOp = 3
	// ExecResize changes the pty size to TTY.
	ExecResize ExecOp = 4

	// ExecStarted reports the PID of the started command.
	ExecStarted ExecOp = 16
	// ExecStdout carries Data from the command's stdout (or its pty).
	ExecStdout ExecOp = 17
	// ExecStderr carries Data from the command's stderr.
	ExecStderr ExecOp = 18
	// ExecExit carries Exit when the command's main process ends. Output
	// frames may still follow while other processes (background children)
	// keep the output pipes open, as they would for a local command; the
	// server closes the stream once the pipes reach EOF.
	ExecExit ExecOp = 19
)

// ExecFrame is every frame after ExecStart, in both directions.
//
// Signal numbers are Linux generic numbers, which are the same on every
// architecture tele supports.
type ExecFrame struct {
	Op     ExecOp      `cbor:"1,keyasint"`
	Data   []byte      `cbor:"2,keyasint,omitempty"`
	Signal int         `cbor:"3,keyasint,omitempty"`
	TTY    *TTYSize    `cbor:"4,keyasint,omitempty"`
	PID    int         `cbor:"5,keyasint,omitempty"`
	Exit   *ExecStatus `cbor:"6,keyasint,omitempty"`
}

// ExecStatus reports how a remote command ended.
type ExecStatus struct {
	// Code is the exit code when the command exited normally.
	Code int `cbor:"1,keyasint,omitempty"`
	// Signal is the terminating signal, or 0 if the command exited.
	Signal int `cbor:"2,keyasint,omitempty"`
	// WatchSeq is the exec barrier: the client must apply every
	// WatchEvent with Seq <= WatchSeq before reporting the exit
	// (docs/exec.md section 6).
	WatchSeq uint64 `cbor:"3,keyasint,omitempty"`
	// Scratch lists scratch files that changed while the command ran.
	Scratch []ScratchFile `cbor:"4,keyasint,omitempty"`
	// Err is set when the command could not be started (for example
	// ENOENT for a missing program); Code and Signal are then meaningless.
	Err *Error `cbor:"5,keyasint,omitempty"`
}

// ScratchArea identifies one of the scratch prefixes (docs/exec.md
// section 5).
type ScratchArea uint8

// Scratch areas.
const (
	// ScratchTmp is CLAUDE_CODE_TMPDIR.
	ScratchTmp ScratchArea = 1
	// ScratchSnapshots is <config>/shell-snapshots.
	ScratchSnapshots ScratchArea = 2
	// ScratchSessionEnv is <config>/session-env.
	ScratchSessionEnv ScratchArea = 3
)

// ScratchAreas lists every scratch area in a stable order.
var ScratchAreas = []ScratchArea{ScratchTmp, ScratchSnapshots, ScratchSessionEnv}

// Valid reports whether a is a known scratch area.
func (a ScratchArea) Valid() bool {
	return a >= ScratchTmp && a <= ScratchSessionEnv
}

// Dir is the directory name of the area inside a session scratch directory.
func (a ScratchArea) Dir() string {
	switch a {
	case ScratchTmp:
		return "tmp"
	case ScratchSnapshots:
		return "shell-snapshots"
	case ScratchSessionEnv:
		return "session-env"
	default:
		return fmt.Sprintf("area-%d", uint8(a))
	}
}

// ScratchFile is a small file inside a scratch area.
type ScratchFile struct {
	Area ScratchArea `cbor:"1,keyasint"`
	// Path is slash-separated and relative to the area root; see
	// CheckRelPath.
	Path string `cbor:"2,keyasint"`
	// Mode holds the permission bits.
	Mode uint32 `cbor:"3,keyasint,omitempty"`
	Data []byte `cbor:"4,keyasint,omitempty"`
	// Deleted reports that the file was removed; Data and Mode are unused.
	Deleted bool `cbor:"5,keyasint,omitempty"`
}

// CheckScratch validates scratch files received from a peer.
func CheckScratch(files []ScratchFile) error {
	total := 0
	for i := range files {
		f := &files[i]
		if !f.Area.Valid() {
			return fmt.Errorf("proto: scratch file %q: invalid area %d", f.Path, f.Area)
		}
		if err := CheckRelPath(f.Path); err != nil {
			return err
		}
		if len(f.Data) > ScratchFileMax {
			return fmt.Errorf("proto: scratch file %q: %d bytes exceeds %d", f.Path, len(f.Data), ScratchFileMax)
		}
		total += len(f.Data)
	}
	if total > ScratchTotalMax {
		return fmt.Errorf("proto: scratch files total %d bytes exceeds %d", total, ScratchTotalMax)
	}
	return nil
}
