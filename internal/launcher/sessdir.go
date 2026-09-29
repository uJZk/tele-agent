package launcher

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ujzk/tele-agent/internal/cabundle"
	"github.com/ujzk/tele-agent/internal/shimsrv"
)

// Files and directories in the session directory (docs/architecture.md
// "进程与角色"). The token and log names belong to shimsrv, whose peer, the
// shim, relies on them.
const (
	caBundleFile     = "ca-bundle.pem"
	systemPromptFile = "system-prompt.md"
	binDir           = "bin"
	libDir           = "lib"
	tmpDir           = "tmp"
)

// sessDirSpec holds what prepareSessionDir writes.
type sessDirSpec struct {
	Dir          string   // session directory in session main's view
	UserEnv      []string // the environment tele was started with
	Token        []byte   // session token
	SystemPrompt string   // composed appended system prompt
	LogLevel     slog.Level
	// Warn receives warnings the user must see before Claude starts, such
	// as an ignored CA path. It is tele's own stderr, never a shim's.
	Warn io.Writer
}

// sessionDir is a prepared session directory.
type sessionDir struct {
	Dir string
	Log *slog.Logger
	log *os.File
}

// Close closes the session log.
func (s *sessionDir) Close() error {
	return s.log.Close()
}

// prepareSessionDir creates the session directory and fills it: the
// subdirectories, the session token, session main's log, the CA bundle and
// the system prompt file (docs/cli.md "启动流程"). On error the directory is
// removed again.
func prepareSessionDir(s sessDirSpec) (_ *sessionDir, err error) {
	// The directory must not exist yet: a leftover one could hold a token
	// or log of another session.
	if err := os.Mkdir(s.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(s.Dir) // ours, created above
		}
	}()
	for _, d := range []string{binDir, libDir, tmpDir} {
		if err := os.Mkdir(filepath.Join(s.Dir, d), 0o700); err != nil {
			return nil, fmt.Errorf("create session directory: %w", err)
		}
	}
	if err := writeNew(filepath.Join(s.Dir, shimsrv.TokenFile), s.Token); err != nil {
		return nil, fmt.Errorf("write session token: %w", err)
	}
	// docs/exec.md "shim 与会话主进程": a shim that loses its connection
	// points the user at this file, so session main logs nowhere else.
	logf, err := os.OpenFile(filepath.Join(s.Dir, shimsrv.LogFile),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create session log: %w", err)
	}
	sd := &sessionDir{
		Dir: s.Dir,
		Log: slog.New(slog.NewTextHandler(logf, &slog.HandlerOptions{Level: s.LogLevel})),
		log: logf,
	}
	defer func() {
		if err != nil {
			_ = sd.Close() // the directory is removed anyway
		}
	}()
	if err := writeCABundle(s.Dir, s.UserEnv, sd.Log, s.Warn); err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(s.Dir, systemPromptFile), []byte(s.SystemPrompt)); err != nil {
		return nil, fmt.Errorf("write system prompt: %w", err)
	}
	return sd, nil
}

// writeCABundle writes the CA bundle that SSL_CERT_FILE and
// NODE_EXTRA_CA_CERTS name (docs/filesystem.md "本地集合"). CA paths the user
// configured but that cannot be read only produce warnings, as they do for
// plain claude.
func writeCABundle(dir string, userEnv []string, log *slog.Logger, warn io.Writer) error {
	pem, skipped, err := cabundle.Build(caExtras(userEnv))
	for _, e := range skipped {
		log.Warn("CA certificates ignored", "err", e)
		if warn != nil {
			_, _ = fmt.Fprintf(warn, "tele: warning: %v\n", e) // best effort, before Claude starts
		}
	}
	if err != nil {
		return err
	}
	if err := writeNew(filepath.Join(dir, caBundleFile), pem); err != nil {
		return fmt.Errorf("write CA bundle: %w", err)
	}
	return nil
}

// caExtras returns the CA paths of the user's environment. SSL_CERT_DIR is a
// colon-separated list, as OpenSSL and Go read it.
func caExtras(userEnv []string) []cabundle.Extra {
	var out []cabundle.Extra
	for _, kv := range userEnv {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS":
			out = append(out, cabundle.Extra{Var: k, Path: v})
		case "SSL_CERT_DIR":
			for d := range strings.SplitSeq(v, ":") {
				out = append(out, cabundle.Extra{Var: k, Path: d})
			}
		}
	}
	return out
}

// writeNew creates path with mode 0600 and writes data; path must not exist.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}
