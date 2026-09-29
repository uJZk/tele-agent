// Package servercmd implements "tele server": the tele server on the target
// host (docs/cli.md "安装与配对").
package servercmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ujzk/tele-agent/internal/endpoint"
	"github.com/ujzk/tele-agent/internal/server"
	"github.com/ujzk/tele-agent/internal/servercfg"
	"github.com/ujzk/tele-agent/internal/sstransport"
	"github.com/ujzk/tele-agent/internal/version"
)

// Usage is the synopsis of "tele server".
const Usage = `usage: tele server run [--config <file>] [--listen <addr>] [--debug]
       tele server install [--pair <tele1:…> | --pair-file <file>] [--listen <addr>] [--yes | --check | --print-commands]
       tele server uninstall [--yes]`

// Streams are the process's standard streams; tests replace them.
type Streams struct {
	// In is the standard input, used when it is a terminal.
	In *os.File
	// Terminal, if set, is where consent is asked and answered; nil when
	// the standard input is not a terminal.
	Terminal io.ReadWriter
	Out, Err io.Writer
}

// Main runs "tele server <args>" and returns the exit status.
func Main(args []string, st Streams) int {
	stdout, stderr := st.Out, st.Err
	ctx := context.Background()
	if len(args) == 0 {
		fmt.Fprintln(stderr, Usage)
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		err = run(args[1:], stderr)
	case "install":
		err = install(ctx, args[1:], st)
	case "uninstall":
		err = uninstall(ctx, args[1:], st)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, Usage)
		return 0
	default:
		fmt.Fprintf(stderr, "tele server: unknown command %q\n%s\n", args[0], Usage)
		return 2
	}
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(stderr, "tele server: %v\n%s\n", err, Usage)
		return 2
	default:
		fmt.Fprintf(stderr, "tele server: %v\n", err)
		return 1
	}
}

type usageError struct{ error }

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	return nil
}

// run serves sessions until SIGTERM or SIGINT. Its log goes to stderr,
// which systemd sends to the journal.
func run(args []string, stderr io.Writer) error {
	fs := newFlagSet("run")
	cfgPath := fs.String("config", "", "configuration file")
	listen := fs.String("listen", "", "TCP address to listen on, overriding the configuration")
	debug := fs.Bool("debug", false, "log debug messages")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *cfgPath == "" {
		p, err := servercfg.Path()
		if err != nil {
			return err
		}
		*cfgPath = p
	}
	cfg, psk, err := servercfg.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	err = serve(ctx, cfg.Listen, psk, log, nil)
	if ctx.Err() != nil {
		return nil // stopped by a signal
	}
	return err
}

// serve listens on addr and serves sessions until ctx ends. ready, if set,
// receives the bound address once listening.
func serve(ctx context.Context, addr string, psk sstransport.PSK, log *slog.Logger, ready func(string)) error {
	ep := endpoint.Endpoint{Network: endpoint.NetworkSS2022, Address: addr}.WithPSK(psk)
	ln, err := ep.Listen(ctx, log.With("layer", "ss2022"))
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	log.Info("tele server started", "version", version.String(), "listen", ln.Addr().String(), "pid", os.Getpid())
	if ready != nil {
		ready(ln.Addr().String())
	}
	srv := server.New(server.Config{TransportAuthenticated: true, Logger: log})
	err = srv.Serve(ctx, ln)
	log.Info("tele server stopped")
	return err
}
