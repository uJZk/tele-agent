package servercmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"strconv"

	"github.com/ujzk/tele-agent/internal/manifest"
	"github.com/ujzk/tele-agent/internal/pairing"
	"github.com/ujzk/tele-agent/internal/preflight"
	"github.com/ujzk/tele-agent/internal/servercfg"
)

// installOptions are the options of install and uninstall.
type installOptions struct {
	pair, pairFile, listen string
	mode                   preflight.Mode
}

// modeFlags registers --yes, --check and --print-commands.
func modeFlags(fs interface {
	Bool(string, bool, string) *bool
}) func() (preflight.Mode, error) {
	yes := fs.Bool("yes", false, "consent to every change that needs consent")
	check := fs.Bool("check", false, "only check")
	printCmds := fs.Bool("print-commands", false, "print the commands that need consent, run nothing")
	return func() (preflight.Mode, error) {
		n := 0
		mode := preflight.Repair
		for _, f := range []struct {
			set  bool
			mode preflight.Mode
		}{{*yes, preflight.Yes}, {*check, preflight.CheckOnly}, {*printCmds, preflight.PrintCommands}} {
			if f.set {
				n++
				mode = f.mode
			}
		}
		if n > 1 {
			return 0, usageError{errors.New("--yes, --check and --print-commands exclude each other")}
		}
		return mode, nil
	}
}

// install pairs this host and installs tele server (docs/cli.md "安装与配对").
func install(ctx context.Context, args []string, st Streams) error {
	fs := newFlagSet("install")
	var o installOptions
	fs.StringVar(&o.pair, "pair", "", "the pairing string (for automation: it stays in the shell history)")
	fs.StringVar(&o.pairFile, "pair-file", "", "read the pairing string from this file, then delete it")
	fs.StringVar(&o.listen, "listen", "", "TCP address to listen on, when a NAT maps the port")
	mode := modeFlags(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	var err error
	if o.mode, err = mode(); err != nil {
		return err
	}
	if o.pair != "" && o.pairFile != "" {
		return usageError{errors.New("--pair and --pair-file exclude each other")}
	}
	if o.listen != "" {
		if _, _, err := net.SplitHostPort(o.listen); err != nil {
			return usageError{fmt.Errorf("invalid --listen %q: %w", o.listen, err)}
		}
	}

	cfgPath, err := servercfg.Path()
	if err != nil {
		return err
	}
	manPath, err := manifest.Path()
	if err != nil {
		return err
	}
	man, err := manifest.Load(manPath)
	if err != nil {
		return err
	}
	env, err := newInstallEnv(man, cfgPath)
	if err != nil {
		return err
	}
	cur, _, curErr := servercfg.Load(cfgPath)

	offer, err := readOffer(o, cur != nil, st)
	if err != nil {
		return err
	}
	switch {
	case offer != nil:
		env.want = &servercfg.Config{Listen: ":" + strconv.Itoa(int(offer.Port)), PSK: offer.PSK.Encode()}
		if o.listen != "" {
			env.want.Listen = o.listen
		}
		if cur != nil && cur.PSK != env.want.PSK && o.mode != preflight.CheckOnly && o.mode != preflight.PrintCommands {
			if err := confirmReplace(o.mode, st); err != nil {
				return err
			}
		}
	case o.listen != "" && cur != nil:
		want := *cur
		want.Listen = o.listen
		env.want = &want
	case cur == nil && errors.Is(curErr, servercfg.ErrNotConfigured) && o.mode != preflight.CheckOnly && o.mode != preflight.PrintCommands:
		return errors.New("not paired yet: pass the pairing string from `tele host add` (paste it when asked, or use --pair-file)")
	}

	r := &preflight.Runner{Mode: o.mode, Out: st.Out, Terminal: st.Terminal, Recorder: man}
	ok := r.Run(ctx, env.checks())
	if offer != nil {
		if now, _, err := servercfg.Load(cfgPath); err == nil && now.PSK == env.want.PSK {
			if err := printReceipt(st.Out, offer, now.Listen); err != nil {
				return err
			}
		}
	}
	if !ok {
		return errors.New("installation incomplete; see above")
	}
	return nil
}

// readOffer returns the pairing offer from --pair, --pair-file or the
// terminal, or nil when none is given and the server is configured.
func readOffer(o installOptions, configured bool, st Streams) (*pairing.Offer, error) {
	var s string
	switch {
	case o.pair != "":
		s = o.pair
	case o.pairFile != "":
		b, err := os.ReadFile(o.pairFile)
		if err != nil {
			return nil, err
		}
		// Deleted even when it does not parse: it may still hold a key.
		if err := os.Remove(o.pairFile); err != nil {
			return nil, err
		}
		s = string(b)
	case st.Terminal != nil && st.In != nil && o.mode != preflight.CheckOnly && o.mode != preflight.PrintCommands:
		if configured {
			fmt.Fprint(st.Terminal, "Paste the pairing string from `tele host add` (input is hidden; empty keeps the current key): ")
		} else {
			fmt.Fprint(st.Terminal, "Paste the pairing string from `tele host add` (input is hidden): ")
		}
		line, err := preflight.ReadSecret(st.In)
		if err != nil {
			return nil, fmt.Errorf("read the pairing string: %w", err)
		}
		s = line
	}
	if s == "" {
		return nil, nil
	}
	return pairing.ParseOffer(s)
}

// confirmReplace asks before a new key replaces the current one: the
// clients paired with it stop working (docs/cli.md "安装与配对").
func confirmReplace(mode preflight.Mode, st Streams) error {
	const msg = "tele server is already paired with another key; the new key replaces it and the clients paired before stop working"
	switch {
	case mode == preflight.Yes:
		fmt.Fprintln(st.Out, msg+" (--yes).")
		return nil
	case st.Terminal == nil:
		return errors.New(msg + "; pass --yes to replace it")
	}
	fmt.Fprint(st.Terminal, msg+". Replace it? [y/N] ")
	line, err := preflight.ReadLine(st.Terminal)
	if err != nil || (line != "y" && line != "Y" && line != "yes") {
		return errors.New("kept the current key")
	}
	return nil
}

func printReceipt(w io.Writer, o *pairing.Offer, listen string) error {
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return err
	}
	u, host := "", ""
	if h, err := os.Hostname(); err == nil {
		host = h
	}
	if cu, err := user.Current(); err == nil {
		u = cu.Username
	}
	s, err := pairing.NewReceipt(o, uint16(port), u, host).Encode()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "\nPaired. Confirm on the local side with `tele host confirm <alias>` and this receipt:\n\n  %s\n\n", s)
	return err
}

// uninstall rolls back the install manifest and deletes the configuration,
// which revokes the key (docs/security.md "远程代码执行入口").
func uninstall(ctx context.Context, args []string, st Streams) error {
	fs := newFlagSet("uninstall")
	yes := fs.Bool("yes", false, "consent to every rollback that needs consent")
	if err := parse(fs, args); err != nil {
		return err
	}
	manPath, err := manifest.Path()
	if err != nil {
		return err
	}
	man, err := manifest.Load(manPath)
	if err != nil {
		return err
	}
	cfgPath, err := servercfg.Path()
	if err != nil {
		return err
	}
	var left []manifest.Entry
	for i := len(man.Entries) - 1; i >= 0; i-- {
		e := man.Entries[i]
		if err := rollback(ctx, e, *yes, st); err != nil {
			fmt.Fprintf(st.Out, "❌ %s: %v\n", e.Desc, err)
			left = append(left, e)
			continue
		}
		fmt.Fprintf(st.Out, "✅ undone: %s\n", e.Desc)
		if err := man.Remove(e.ID); err != nil {
			return err
		}
	}
	// The configuration goes even if install did not write it: deleting
	// it is what revokes the key.
	if err := os.Remove(cfgPath); err == nil {
		fmt.Fprintf(st.Out, "✅ deleted %s: the key is revoked\n", cfgPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(man.BackupDir()) // only when empty
	if len(left) > 0 {
		fmt.Fprintln(st.Out, "\nNot undone; run these commands yourself, or run `tele server uninstall` again:")
		for _, e := range left {
			if e.Undo != "" {
				fmt.Fprintf(st.Out, "\n  # %s\n    %s\n", e.Desc, e.Undo)
			}
		}
		return errors.New("uninstall incomplete; see above")
	}
	fmt.Fprintln(st.Out, "tele server is uninstalled. A server started by hand with `tele server run` keeps running until stopped.")
	return nil
}

// rollback undoes one entry. An Undo that needs consent runs only with it.
func rollback(ctx context.Context, e manifest.Entry, yes bool, st Streams) error {
	if e.Undo != "" {
		if e.Consent {
			switch {
			case yes:
			case st.Terminal == nil:
				return errors.New("needs consent: no terminal to ask (use --yes)")
			default:
				fmt.Fprintf(st.Terminal, "Undo %q by running:\n    %s\nRun it? [y/N] ", e.Desc, e.Undo)
				line, err := preflight.ReadLine(st.Terminal)
				if err != nil || (line != "y" && line != "Y" && line != "yes") {
					return errors.New("declined")
				}
			}
		}
		if err := preflight.RunShell(ctx, e.Undo, st.Terminal, st.Out); err != nil {
			return err
		}
	}
	return manifest.RestoreFile(e)
}
