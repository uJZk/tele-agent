package hostcmd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/hostcfg"
	"github.com/ujzk/tele-agent/internal/pairing"
)

// sshProgram is the ssh client; tests replace it.
var sshProgram = "ssh"

// Remote paths used by "tele host add --ssh". The pairing file holds the
// PSK: it is created with umask 077, never passed on a command line, and
// tele server install deletes it after reading it.
const (
	remoteBin     = "~/.local/bin/tele"
	remotePairing = "~/.config/tele/pairing.tmp"
)

// unameArch maps `uname -m` to GOARCH.
var unameArch = map[string]string{
	"x86_64": "amd64", "amd64": "amd64",
	"aarch64": "arm64", "arm64": "arm64",
	"i686": "386", "i386": "386",
	"armv7l": "arm", "armv6l": "arm",
	"riscv64": "riscv64", "ppc64le": "ppc64le", "s390x": "s390x",
}

// ssh runs command on dest through the ssh client. tty requests a
// terminal, so that tele server install can ask for consent.
func ssh(ctx context.Context, dest, command string, tty bool, stdin io.Reader, stdout, stderr io.Writer) error {
	args := []string{"-o", "BatchMode=no"}
	if tty {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
	}
	args = append(args, "--", dest, command)
	cmd := exec.CommandContext(ctx, sshProgram, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh %s %q: %w", dest, command, err)
	}
	return nil
}

// addOverSSH installs this tele binary on dest, runs tele server install
// with the offer and confirms the pairing with the receipt it prints.
func addOverSSH(ctx context.Context, dest string, h *hostcfg.Host, offer *pairing.Offer, st Streams) error {
	var out bytes.Buffer
	if err := ssh(ctx, dest, "uname -m", false, nil, &out, st.Err); err != nil {
		return err
	}
	machine := strings.TrimSpace(out.String())
	if arch := unameArch[machine]; arch != runtime.GOARCH {
		return fmt.Errorf("%s is %s, but this tele is built for %s: install tele for %s there yourself, then pair with `tele host add %s --endpoint %s --force`",
			dest, machine, runtime.GOARCH, machine, h.Alias, h.Endpoint)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	bin, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer bin.Close()
	fmt.Fprintf(st.Err, "Installing tele on %s as %s …\n", dest, remoteBin)
	upload := "mkdir -p ~/.local/bin && cat > " + remoteBin + ".new && chmod 755 " + remoteBin + ".new && mv -f " + remoteBin + ".new " + remoteBin
	if err := ssh(ctx, dest, upload, false, bin, io.Discard, st.Err); err != nil {
		return err
	}

	code, err := offer.Encode()
	if err != nil {
		return err
	}
	store := "umask 077 && mkdir -p ~/.config/tele && cat > " + remotePairing
	if err := ssh(ctx, dest, store, false, strings.NewReader(code+"\n"), io.Discard, st.Err); err != nil {
		return err
	}

	// Show the install's output as it runs and pick the receipt out of it.
	pr, pw := io.Pipe()
	receipt := make(chan string, 1)
	go func() {
		var found string
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			fmt.Fprintln(st.Out, line)
			if l := strings.TrimSpace(line); strings.HasPrefix(l, pairing.ReceiptPrefix) {
				found = l
			}
		}
		_, _ = io.Copy(io.Discard, pr)
		receipt <- found
	}()
	install := remoteBin + " server install --pair-file " + remotePairing
	runErr := ssh(ctx, dest, install, isTerminal(st.In), st.In, pw, st.Err)
	_ = pw.Close()
	found := <-receipt
	if runErr != nil {
		return fmt.Errorf("tele server install on %s failed: %w", dest, runErr)
	}
	if found == "" {
		return fmt.Errorf("tele server install on %s printed no receipt", dest)
	}
	r, err := pairing.ParseReceipt(found)
	if err != nil {
		return err
	}
	return finishPairing(ctx, h, r, st)
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}
