// Proof of concept for feasibility study section 5A.3 (not production code).
// Usage (as a normal user): go run . <src-dir> <mountpoint>
package main

// Launcher test: stage 1 (outside) re-execs itself in new user+mount ns with
// uid map self->self and ambient CAP_SYS_ADMIN; stage 2 mounts FUSE loopback,
// then runs a child shell with NO capabilities to verify access.
import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const capSysAdmin = 21

func main() {
	if len(os.Args) > 1 && os.Args[1] == "stage2" {
		stage2(os.Args[2], os.Args[3])
		return
	}
	uid, gid := os.Getuid(), os.Getgid()
	cmd := exec.Command("/proc/self/exe", "stage2", os.Args[1], os.Args[2])
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		AmbientCaps: []uintptr{capSysAdmin},
	}
	if err := cmd.Run(); err != nil {
		log.Fatal(err)
	}
}

func stage2(src, dst string) {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		log.Fatal("private: ", err)
	}
	root, err := newRoot(src)
	if err != nil {
		log.Fatal(err)
	}
	srv, err := fs.Mount(dst, root, &fs.Options{MountOptions: fuse.MountOptions{DirectMountStrict: true, FsName: "tele", Debug: os.Getenv("FDEBUG") != ""}})
	if err != nil {
		log.Fatal("mount: ", err)
	}
	// Refresh root inode attrs: the kernel initializes the FUSE root as uid 0,
	// which is unmapped in our userns and makes create() fail with EACCES.
	os.Stat(dst)
	sh := exec.Command("/bin/bash", "-c", `id -u; grep CapEff /proc/self/status; cat `+dst+`/a.txt; echo world > `+dst+`/b.txt; grep -c tele /proc/self/mountinfo`)
	sh.Stdout, sh.Stderr = os.Stdout, os.Stderr
	sh.SysProcAttr = &syscall.SysProcAttr{AmbientCaps: nil}
	if err := sh.Run(); err != nil {
		fmt.Println("child:", err)
	}
	srv.Unmount()
}
