package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ujzk/tele-agent/internal/endpoint"
)

// hostConfig is the configuration of one host alias, stored as
// <config dir>/tele/hosts/<alias>.json. Pairing (docs/cli.md) will write
// it; until then it is written by hand.
type hostConfig struct {
	Endpoint string `json:"endpoint"`
	// Token authenticates the session to the server. It grants shell access
	// to the target user, so the file must not be readable by others.
	Token string `json:"token"`
}

func hostConfigPath(alias string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(dir, "tele", "hosts", alias+".json"), nil
}

// loadHost reads the configuration of alias. endpointOverride, if set,
// replaces the configured endpoint.
func loadHost(alias, endpointOverride string) (endpoint.Endpoint, []byte, error) {
	path, err := hostConfigPath(alias)
	if err != nil {
		return endpoint.Endpoint{}, nil, err
	}
	var cfg hostConfig
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return endpoint.Endpoint{}, nil, fmt.Errorf("unknown host %q: no %s", alias, path)
	case err != nil:
		return endpoint.Endpoint{}, nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return endpoint.Endpoint{}, nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return endpoint.Endpoint{}, nil, fmt.Errorf("%s holds a secret but is accessible by others (mode %v); chmod 600 it", path, fi.Mode().Perm())
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return endpoint.Endpoint{}, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if endpointOverride != "" {
		cfg.Endpoint = endpointOverride
	}
	ep, err := endpoint.Parse(cfg.Endpoint)
	if err != nil {
		return endpoint.Endpoint{}, nil, fmt.Errorf("host %q: %w", alias, err)
	}
	if cfg.Token == "" {
		return endpoint.Endpoint{}, nil, fmt.Errorf("host %q: no token in %s", alias, path)
	}
	return ep, []byte(cfg.Token), nil
}
