// Package servercfg stores the configuration of tele server on the target
// host: ~/.config/tele/server.json (docs/cli.md "配置与状态文件").
//
// It holds the PSK, which grants shell access to the target user, so it is
// written with mode 0600 and refused when others can read it.
package servercfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"github.com/ujzk/tele-agent/internal/sstransport"
)

// Config is the server configuration.
type Config struct {
	// Listen is the TCP address to listen on, such as ":8443".
	Listen string `json:"listen"`
	// PSK authenticates clients (base64).
	PSK string `json:"psk"`
}

// ErrNotConfigured reports a missing configuration file.
var ErrNotConfigured = errors.New("tele server is not configured")

// Path returns the default configuration file.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(dir, "tele", "server.json"), nil
}

// Load reads the configuration at path and checks it.
func Load(path string) (*Config, sstransport.PSK, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, sstransport.PSK{}, fmt.Errorf("%w: no %s; pair it with `tele server install --pair …`", ErrNotConfigured, path)
	}
	if err != nil {
		return nil, sstransport.PSK{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, sstransport.PSK{}, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, sstransport.PSK{}, fmt.Errorf("%s holds a secret but is accessible by others (mode %v); chmod 600 it", path, fi.Mode().Perm())
	}
	var c Config
	if err := json.NewDecoder(f).Decode(&c); err != nil {
		return nil, sstransport.PSK{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return nil, sstransport.PSK{}, fmt.Errorf("%s: invalid listen address %q: %w", path, c.Listen, err)
	}
	psk, err := sstransport.ParsePSK(c.PSK)
	if err != nil {
		return nil, sstransport.PSK{}, fmt.Errorf("%s: %w", path, err)
	}
	return &c, psk, nil
}

// Save writes c to path atomically with mode 0600, creating its directory
// with mode 0700.
func Save(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".server.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
