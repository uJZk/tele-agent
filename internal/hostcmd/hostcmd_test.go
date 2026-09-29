package hostcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ujzk/tele-agent/internal/endpoint"
	"github.com/ujzk/tele-agent/internal/hostcfg"
	"github.com/ujzk/tele-agent/internal/pairing"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/server"
	"github.com/ujzk/tele-agent/internal/sstransport"
)

// fakeSSHEnv makes the test binary act as the ssh client: it runs the
// remote command locally, except "tele server install --pair-file", which
// it answers with a receipt the way the real install does.
const fakeSSHEnv = "TELE_TEST_FAKE_SSH"

func TestMain(m *testing.M) {
	if os.Getenv(fakeSSHEnv) == "1" {
		os.Exit(fakeSSH(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeSSH(args []string) int {
	i := 0
	for i < len(args) && args[i] != "--" {
		i++
	}
	if i+2 >= len(args) {
		fmt.Fprintln(os.Stderr, "fake ssh: bad arguments", args)
		return 255
	}
	command := args[i+2]
	if command == "uname -m" && os.Getenv("TELE_TEST_UNAME") != "" {
		fmt.Println(os.Getenv("TELE_TEST_UNAME"))
		return 0
	}
	if f, ok := strings.CutPrefix(command, remoteBin+" server install --pair-file "); ok {
		path := strings.Replace(f, "~", os.Getenv("HOME"), 1)
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake install:", err)
			return 1
		}
		_ = os.Remove(path)
		o, err := pairing.ParseOffer(string(b))
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake install:", err)
			return 1
		}
		r, _ := pairing.NewReceipt(o, o.Port, "bob", "fakehost").Encode()
		fmt.Printf("installed\r\n%s\r\n", r) // as through a terminal
		return 0
	}
	cmd := exec.CommandContext(context.Background(), "sh", "-c", command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 255
	}
	return 0
}

func runMain(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(args, Streams{In: strings.NewReader(stdin), Out: &out, Err: &errb})
	return code, out.String(), errb.String()
}

var offerRE = regexp.MustCompile(`tele1:[A-Za-z0-9_-]+`)

// freePort returns a TCP port on 127.0.0.1 that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %T", ln.Addr())
	}
	_ = ln.Close()
	return addr.Port
}

// startServer runs tele server with psk on 127.0.0.1:port.
func startServer(t *testing.T, psk sstransport.PSK, port int) {
	t.Helper()
	ep := endpoint.Endpoint{Network: endpoint.NetworkSS2022, Address: "127.0.0.1:" + strconv.Itoa(port)}.WithPSK(psk)
	ln, err := ep.Listen(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Config{
		TransportAuthenticated: true,
		FSRoot:                 t.TempDir(),
		Target:                 &proto.TargetInfo{Hostname: "build1", User: "bob", Home: t.TempDir(), Shell: "/bin/sh", OSPrettyName: "TestOS"},
		ScratchBase:            t.TempDir(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = srv.Serve(ctx, ln) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

func TestPairingFlow(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	port := freePort(t)
	ep := "127.0.0.1:" + strconv.Itoa(port)

	code, out, errOut := runMain(t, "", "add", "dev", "--endpoint", ep)
	if code != 0 {
		t.Fatalf("add: %d %s", code, errOut)
	}
	offerStr := offerRE.FindString(out)
	offer, err := pairing.ParseOffer(offerStr)
	if err != nil {
		t.Fatalf("add printed no usable offer: %v\n%s", err, out)
	}
	if !strings.Contains(out, "tele host confirm dev") {
		t.Errorf("add output does not tell how to confirm:\n%s", out)
	}
	if _, _, err := loadDev(t).Resolve(""); !errors.Is(err, hostcfg.ErrPending) {
		t.Fatalf("pending host resolves: %v", err)
	}
	if _, out, _ := runMain(t, "", "ls"); !regexp.MustCompile(`dev\s+\S+\s+pending`).MatchString(out) {
		t.Errorf("ls does not show the pending host:\n%s", out)
	}
	if code, _, errOut := runMain(t, "", "add", "dev", "--endpoint", ep); code == 0 || !strings.Contains(errOut, "--force") {
		t.Errorf("second add without --force: %d %s", code, errOut)
	}

	// A receipt for another offer is refused and the host stays pending.
	other, _ := pairing.NewOffer(uint16(port))
	bad, _ := pairing.NewReceipt(other, uint16(port), "bob", "build1").Encode()
	if code, _, errOut := runMain(t, "", "confirm", "dev", bad); code == 0 || !strings.Contains(errOut, "does not answer this pairing") {
		t.Fatalf("confirm with a foreign receipt: %d %s", code, errOut)
	}

	startServer(t, offer.PSK, port)
	receipt, _ := pairing.NewReceipt(offer, uint16(port), "bob", "build1").Encode()
	// The receipt can also be pasted on stdin.
	code, out, errOut = runMain(t, receipt+"\n", "confirm", "dev")
	if code != 0 || !strings.Contains(out, "Paired \"dev\" with bob@build1") || !strings.Contains(out, "Connected: bob@build1") {
		t.Fatalf("confirm: %d\n%s\n%s", code, out, errOut)
	}
	if _, _, err := loadDev(t).Resolve(""); err != nil {
		t.Fatalf("confirmed host does not resolve: %v", err)
	}
	if _, out, _ := runMain(t, "", "ls"); !regexp.MustCompile(`dev\s+127\.0\.0\.1:\d+\s+ok\s+bob@build1, TestOS`).MatchString(out) {
		t.Errorf("ls does not show the reachable host:\n%s", out)
	}
	if code, _, errOut := runMain(t, "", "confirm", "dev", receipt); code == 0 || !strings.Contains(errOut, "not waiting for pairing") {
		t.Errorf("second confirm: %d %s", code, errOut)
	}

	if code, out, _ := runMain(t, "", "rm", "dev"); code != 0 || !strings.Contains(out, "tele server uninstall") {
		t.Fatalf("rm: %d %s", code, out)
	}
	if _, out, _ := runMain(t, "", "ls"); !strings.Contains(out, "No hosts") {
		t.Errorf("ls after rm:\n%s", out)
	}
}

// loadDev loads the host the tests add.
func loadDev(t *testing.T) *hostcfg.Host {
	t.Helper()
	h, err := hostcfg.Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestAddAlternates(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if code, _, errOut := runMain(t, "", "add", "dev", "--endpoint", "a.example:8443", "--endpoint", "[2001:db8::1]:8443"); code != 0 {
		t.Fatalf("add: %d %s", code, errOut)
	}
	h := loadDev(t)
	if h.Endpoint != "a.example:8443" || len(h.Alternates) != 1 || h.Alternates[0] != "[2001:db8::1]:8443" {
		t.Fatalf("host = %+v", h)
	}
}

func TestAddOverSSH(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	remoteHome := t.TempDir()
	t.Setenv("HOME", remoteHome) // the fake ssh runs "remote" commands here
	t.Setenv(fakeSSHEnv, "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	old := sshProgram
	sshProgram = exe
	t.Cleanup(func() { sshProgram = old })

	code, out, errOut := runMain(t, "", "add", "dev", "--ssh", "bob@127.0.0.1")
	if code != 0 {
		t.Fatalf("add --ssh: %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "Paired \"dev\" with bob@fakehost") {
		t.Errorf("add --ssh did not confirm:\n%s", out)
	}
	h := loadDev(t)
	if h.PendingToken != "" || h.Endpoint != "127.0.0.1:"+strconv.Itoa(pairing.DefaultPort) {
		t.Errorf("host after add --ssh: %+v", h)
	}
	bin, err := os.ReadFile(filepath.Join(remoteHome, ".local", "bin", "tele"))
	self, _ := os.ReadFile(exe)
	if err != nil || !bytes.Equal(bin, self) {
		t.Errorf("remote binary not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteHome, ".config", "tele", "pairing.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pairing file left behind on the remote: %v", err)
	}
}

func TestAddOverSSHWrongArch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(fakeSSHEnv, "1")
	t.Setenv("TELE_TEST_UNAME", "mips")
	exe, _ := os.Executable()
	old := sshProgram
	sshProgram = exe
	t.Cleanup(func() { sshProgram = old })

	code, _, errOut := runMain(t, "", "add", "dev", "--ssh", "127.0.0.1")
	if code == 0 || !strings.Contains(errOut, "is mips") {
		t.Fatalf("add --ssh to another architecture: %d %s", code, errOut)
	}
}

func TestUsage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "usage: tele host add"},
		{[]string{"add"}, 2, "missing host alias"},
		{[]string{"add", "dev"}, 2, "--endpoint or --ssh"},
		{[]string{"add", "host", "--endpoint", "h:1"}, 1, "tele command"},
		{[]string{"add", "dev", "--endpoint", "unix:/s"}, 1, "host:port"},
		{[]string{"confirm", "nope", "tele1r:x"}, 1, "unknown host"},
		{[]string{"rm"}, 2, "exactly one"},
		{[]string{"frob"}, 2, `unknown command "frob"`},
	} {
		code, out, errOut := runMain(t, "", tc.args...)
		if code != tc.code || !strings.Contains(out+errOut, tc.want) {
			t.Errorf("%q = %d %q, want %d containing %q", tc.args, code, out+errOut, tc.code, tc.want)
		}
	}
}
