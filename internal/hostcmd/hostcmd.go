// Package hostcmd implements "tele host": pairing and managing host aliases
// (docs/cli.md "安装与配对").
package hostcmd

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/ujzk/tele-agent/internal/client"
	"github.com/ujzk/tele-agent/internal/endpoint"
	"github.com/ujzk/tele-agent/internal/hostcfg"
	"github.com/ujzk/tele-agent/internal/pairing"
	"github.com/ujzk/tele-agent/internal/resume"
	"github.com/ujzk/tele-agent/internal/sstransport"
)

// Usage is the synopsis of "tele host".
const Usage = `usage: tele host add <alias> (--endpoint <host:port>... | --ssh <[user@]host> [--endpoint <host:port>...]) [--force]
       tele host confirm <alias> [<tele1r:…>]
       tele host ls
       tele host rm <alias>`

// probeTimeout bounds a connectivity check of one host.
const probeTimeout = 10 * time.Second

// Streams are the process's standard streams; tests replace them.
type Streams struct {
	In       io.Reader
	Out, Err io.Writer
}

// Main runs "tele host <args>" and returns the exit status.
func Main(args []string, st Streams) int {
	if len(args) == 0 {
		fmt.Fprintln(st.Err, Usage)
		return 2
	}
	ctx := context.Background()
	var err error
	switch args[0] {
	case "add":
		err = add(ctx, args[1:], st)
	case "confirm":
		err = confirm(ctx, args[1:], st)
	case "ls", "list":
		err = list(ctx, args[1:], st)
	case "rm", "remove":
		err = remove(args[1:], st)
	case "help", "-h", "--help":
		fmt.Fprintln(st.Out, Usage)
		return 0
	default:
		fmt.Fprintf(st.Err, "tele host: unknown command %q\n%s\n", args[0], Usage)
		return 2
	}
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(st.Err, "tele host: %v\n%s\n", err, Usage)
		return 2
	default:
		fmt.Fprintf(st.Err, "tele host: %v\n", err)
		return 1
	}
}

type usageError struct{ error }

// parseAliasFlags parses "<alias> [flags]" or "[flags] <alias>" and returns
// the remaining positional arguments after the alias.
func parseAliasFlags(fs *flag.FlagSet, args []string) (alias string, rest []string, err error) {
	fs.SetOutput(io.Discard)
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		alias, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", nil, usageError{err}
	}
	rest = fs.Args()
	if alias == "" {
		if len(rest) == 0 {
			return "", nil, usageError{errors.New("missing host alias")}
		}
		alias, rest = rest[0], rest[1:]
	}
	return alias, rest, nil
}

func add(ctx context.Context, args []string, st Streams) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	var endpoints stringList
	fs.Var(&endpoints, "endpoint", "host:port the local side connects to; repeat for alternates")
	sshDest := fs.String("ssh", "", "[user@]host to install tele server on over SSH")
	force := fs.Bool("force", false, "replace an existing alias")
	alias, rest, err := parseAliasFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", rest[0])}
	}
	if len(endpoints) == 0 {
		if *sshDest == "" {
			return usageError{errors.New("--endpoint or --ssh is required")}
		}
		endpoints = stringList{net.JoinHostPort(sshHost(*sshDest), strconv.Itoa(pairing.DefaultPort))}
	}
	var parsed []endpoint.Endpoint
	for _, s := range endpoints {
		e, err := endpoint.Parse(s)
		if err != nil {
			return err
		}
		if !e.Authenticates() {
			return errors.New("pairing needs host:port endpoints")
		}
		parsed = append(parsed, e)
	}
	e := parsed[0]
	_, portStr, _ := net.SplitHostPort(e.Address)
	port, _ := strconv.ParseUint(portStr, 10, 16) // validated by Parse

	if _, err := hostcfg.Load(alias); err == nil && !*force {
		return fmt.Errorf("host %q exists; use --force to pair it again, which replaces its key", alias)
	} else if err != nil && !errors.Is(err, hostcfg.ErrUnknownHost) && !*force {
		return err
	}
	offer, err := pairing.NewOffer(uint16(port))
	if err != nil {
		return err
	}
	var alternates []string
	for _, a := range parsed[1:] {
		alternates = append(alternates, a.String())
	}
	h := &hostcfg.Host{
		Alias:        alias,
		Endpoint:     e.String(),
		Alternates:   alternates,
		PSK:          offer.PSK.Encode(),
		PendingToken: base64.StdEncoding.EncodeToString(offer.Token),
	}
	if err := hostcfg.Save(h); err != nil {
		return err
	}
	if *sshDest != "" {
		return addOverSSH(ctx, *sshDest, h, offer, st)
	}
	code, err := offer.Encode()
	if err != nil {
		return err
	}
	fmt.Fprintf(st.Out, `Host %q is waiting for pairing. On %s, as the user tele should run as:

  1. Install the tele binary for that host's architecture as ~/.local/bin/tele
     (https://github.com/ujzk/tele-agent/releases).
  2. Run "tele server install" and paste this pairing string when asked:

     %s

     It contains the key to that account: do not share it, and prefer
     pasting it over passing it with --pair, which leaves it in the shell
     history.

Then confirm here with the receipt it prints:

  tele host confirm %s 'tele1r:…'
`, alias, sshHost(e.Address), code, alias)
	return nil
}

func confirm(ctx context.Context, args []string, st Streams) error {
	fs := flag.NewFlagSet("confirm", flag.ContinueOnError)
	alias, rest, err := parseAliasFlags(fs, args)
	if err != nil {
		return err
	}
	var receipt string
	switch len(rest) {
	case 0:
		fmt.Fprint(st.Err, "Paste the receipt printed by `tele server install`: ")
		line, err := readLine(st.In)
		if err != nil {
			return fmt.Errorf("read receipt: %w", err)
		}
		receipt = line
	case 1:
		receipt = rest[0]
	default:
		return usageError{fmt.Errorf("unexpected argument %q", rest[1])}
	}
	h, err := hostcfg.Load(alias)
	if err != nil {
		return err
	}
	r, err := pairing.ParseReceipt(receipt)
	if err != nil {
		return err
	}
	return finishPairing(ctx, h, r, st)
}

// finishPairing verifies the receipt against the pending pairing of h,
// marks h confirmed and checks that the server can be reached.
func finishPairing(ctx context.Context, h *hostcfg.Host, r *pairing.Receipt, st Streams) error {
	if h.PendingToken == "" {
		return fmt.Errorf("host %q is not waiting for pairing; pair it again with `tele host add %s --force`", h.Alias, h.Alias)
	}
	token, err := base64.StdEncoding.DecodeString(h.PendingToken)
	if err != nil {
		return fmt.Errorf("host %q: damaged pending pairing; pair it again with `tele host add %s --force`", h.Alias, h.Alias)
	}
	psk, err := sstransport.ParsePSK(h.PSK)
	if err != nil {
		return fmt.Errorf("host %q: %w", h.Alias, err)
	}
	if err := r.Verify(psk, token); err != nil {
		return err
	}
	h.PendingToken = ""
	if err := hostcfg.Save(h); err != nil {
		return err
	}
	fmt.Fprintf(st.Out, "Paired %q with %s@%s.\n", h.Alias, r.User, r.Hostname)
	if _, port, err := net.SplitHostPort(h.Endpoint); err == nil && port != strconv.Itoa(int(r.Port)) {
		fmt.Fprintf(st.Out, "Note: the server listens on port %d, but %q connects to %s; that works only if a NAT or firewall forwards the port.\n", r.Port, h.Alias, h.Endpoint)
	}
	// The pairing holds even when the server cannot be reached yet (a
	// firewall still closed, a service not started): that is a warning.
	if p := probe(ctx, h); p.err == nil {
		fmt.Fprintf(st.Out, "Connected: %s\n", p.summary())
	} else {
		fmt.Fprintf(st.Out, "Warning: cannot connect yet: %v\nCheck that the server runs (`tele server install` starts it) and that its port is reachable, then run `tele doctor %s`.\n", p.err, h.Alias)
	}
	return nil
}

func list(ctx context.Context, args []string, st Streams) error {
	if len(args) > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", args[0])}
	}
	aliases, err := hostcfg.List()
	if err != nil {
		return err
	}
	if len(aliases) == 0 {
		fmt.Fprintln(st.Out, "No hosts. Pair one with `tele host add <alias> --endpoint <host:port>`.")
		return nil
	}
	rows := make([]string, len(aliases))
	var wg sync.WaitGroup
	for i, alias := range aliases {
		wg.Go(func() {
			h, err := hostcfg.Load(alias)
			switch {
			case err != nil:
				rows[i] = fmt.Sprintf("%s\t-\terror\t%v", alias, err)
			case h.PendingToken != "":
				rows[i] = fmt.Sprintf("%s\t%s\tpending\trun `tele host confirm %s`", alias, h.Endpoint, alias)
			default:
				if p := probe(ctx, h); p.err != nil {
					rows[i] = fmt.Sprintf("%s\t%s\tunreachable\t%v", alias, h.Endpoint, p.err)
				} else {
					rows[i] = fmt.Sprintf("%s\t%s\tok\t%s", alias, h.Endpoint, p.summary())
				}
			}
		})
	}
	wg.Wait()
	tw := tabwriter.NewWriter(st.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ALIAS\tENDPOINT\tSTATUS\tDETAILS")
	for _, r := range rows {
		fmt.Fprintln(tw, r)
	}
	return tw.Flush()
}

func remove(args []string, st Streams) error {
	if len(args) != 1 {
		return usageError{errors.New("want exactly one host alias")}
	}
	if err := hostcfg.Remove(args[0]); err != nil {
		return err
	}
	fmt.Fprintf(st.Out, "Removed %q and its local key. The server still accepts that key: run `tele server uninstall` on it, or pair it again, to revoke it.\n", args[0])
	return nil
}

// probeResult is the outcome of a connectivity check.
type probeResult struct {
	sess *client.Session
	err  error
}

func (p probeResult) summary() string {
	t := p.sess.Target
	return fmt.Sprintf("%s@%s, %s %s, rtt %v, clock skew %v", t.User, t.Hostname, t.OSPrettyName, t.Arch,
		p.sess.RTT.Round(time.Millisecond), p.sess.ClockSkew.Round(100*time.Millisecond))
}

// probe opens and closes a session with h.
func probe(ctx context.Context, h *hostcfg.Host) probeResult {
	eps, token, err := h.ResolveAll("")
	if err != nil {
		return probeResult{err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	s, err := client.ConnectDial(ctx, client.Rotate(eps), token, resume.Config{})
	if err != nil {
		return probeResult{err: err}
	}
	_ = s.Close()
	return probeResult{sess: s}
}

// readLine reads one line from r without buffering beyond it.
func readLine(r io.Reader) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for b.Len() < 8192 {
		n, err := r.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return strings.TrimSpace(b.String()), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && b.Len() > 0 {
				return strings.TrimSpace(b.String()), nil
			}
			return "", err
		}
	}
	return "", errors.New("line too long")
}

// sshHost returns the host part of "[user@]host[:port]".
func sshHost(dest string) string {
	if _, h, ok := strings.Cut(dest, "@"); ok {
		dest = h
	}
	if h, _, err := net.SplitHostPort(dest); err == nil {
		return h
	}
	return dest
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}
