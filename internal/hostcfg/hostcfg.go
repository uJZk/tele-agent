// Package hostcfg stores the local configuration of each host alias:
// <config dir>/tele/hosts/<alias>.json (docs/cli.md "配置与状态文件").
//
// A host file holds a secret that grants shell access to the target user,
// so it is written with mode 0600 and refused when others can read it.
package hostcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ujzk/tele-agent/internal/cli"
	"github.com/ujzk/tele-agent/internal/endpoint"
	"github.com/ujzk/tele-agent/internal/sstransport"
)

// Host is the configuration of one alias.
type Host struct {
	Alias string `json:"-"`
	// Endpoint is where the server listens: host:port (SS2022) or
	// unix:<path>.
	Endpoint string `json:"endpoint"`
	// Alternates are further host:port addresses of the same server, with
	// the same PSK, such as another address family or another forwarded
	// port. The session rotates among them when dialing fails
	// (docs/transport.md "可恢复会话层").
	Alternates []string `json:"alternates,omitempty"`
	// PSK authenticates an SS2022 endpoint (base64).
	PSK string `json:"psk,omitempty"`
	// Token authenticates sessions on a unix endpoint, whose transport
	// does not authenticate the peer.
	Token string `json:"token,omitempty"`
	// PendingToken is the one-time pairing token (base64) while pairing
	// waits for "tele host confirm"; the alias cannot be used until then.
	PendingToken string `json:"pending_token,omitempty"`
}

// ErrPending reports an alias whose pairing is not confirmed yet.
var ErrPending = errors.New("pairing not confirmed")

// ErrUnknownHost reports an alias without a configuration file.
var ErrUnknownHost = errors.New("unknown host")

// Dir returns the directory of the host files.
func Dir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(dir, "tele", "hosts"), nil
}

func path(alias string) (string, error) {
	if err := cli.CheckAlias(alias); err != nil {
		return "", err
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, alias+".json"), nil
}

// Load reads the configuration of alias.
func Load(alias string) (*Host, error) {
	p, err := path(alias)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w %q: no %s; pair it with `tele host add %s`", ErrUnknownHost, alias, p, alias)
	case err != nil:
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s holds a secret but is accessible by others (mode %v); chmod 600 it", p, fi.Mode().Perm())
	}
	var h Host
	if err := json.NewDecoder(f).Decode(&h); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	h.Alias = alias
	return &h, nil
}

// Resolve returns the primary endpoint with its credentials; see
// ResolveAll.
func (h *Host) Resolve(endpointOverride string) (ep endpoint.Endpoint, token []byte, err error) {
	eps, token, err := h.ResolveAll(endpointOverride)
	if err != nil {
		return endpoint.Endpoint{}, nil, err
	}
	return eps[0], token, nil
}

// ResolveAll returns the endpoints to connect to, primary first, with
// their credentials: endpointOverride, if set, replaces them all. token is
// set only for a unix endpoint.
func (h *Host) ResolveAll(endpointOverride string) (eps []endpoint.Endpoint, token []byte, err error) {
	primary, token, err := h.resolve(endpointOverride)
	if err != nil {
		return nil, nil, err
	}
	eps = []endpoint.Endpoint{primary}
	if endpointOverride != "" {
		return eps, token, nil
	}
	for _, a := range h.Alternates {
		ep, err := endpoint.Parse(a)
		if err != nil {
			return nil, nil, fmt.Errorf("host %q: alternate endpoint: %w", h.Alias, err)
		}
		if ep.Network != primary.Network {
			return nil, nil, fmt.Errorf("host %q: alternate endpoint %v is not of the same kind as %v", h.Alias, ep, primary)
		}
		if ep.Authenticates() {
			psk, _ := sstransport.ParsePSK(h.PSK) // checked by resolve
			ep = ep.WithPSK(psk)
		}
		eps = append(eps, ep)
	}
	return eps, token, nil
}

func (h *Host) resolve(endpointOverride string) (ep endpoint.Endpoint, token []byte, err error) {
	if h.PendingToken != "" {
		return endpoint.Endpoint{}, nil, fmt.Errorf("host %q: %w; run `tele host confirm %s <receipt>` with the receipt `tele server install` printed", h.Alias, ErrPending, h.Alias)
	}
	s := h.Endpoint
	if endpointOverride != "" {
		s = endpointOverride
	}
	ep, err = endpoint.Parse(s)
	if err != nil {
		return endpoint.Endpoint{}, nil, fmt.Errorf("host %q: %w", h.Alias, err)
	}
	if ep.Authenticates() {
		if h.PSK == "" {
			return endpoint.Endpoint{}, nil, fmt.Errorf("host %q: no PSK for SS2022 endpoint %v; pair it again with `tele host add`", h.Alias, ep)
		}
		psk, err := sstransport.ParsePSK(h.PSK)
		if err != nil {
			return endpoint.Endpoint{}, nil, fmt.Errorf("host %q: %w", h.Alias, err)
		}
		return ep.WithPSK(psk), nil, nil
	}
	if h.Token == "" {
		return endpoint.Endpoint{}, nil, fmt.Errorf("host %q: no token for unix endpoint %v", h.Alias, ep)
	}
	return ep, []byte(h.Token), nil
}

// Save writes h atomically with mode 0600.
func Save(h *Host) error {
	p, err := path(h.Alias)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+h.Alias+".*.tmp")
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
	return os.Rename(tmp.Name(), p)
}

// List returns the configured aliases, sorted.
func List() ([]string, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var aliases []string
	for _, e := range entries {
		alias, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || cli.CheckAlias(alias) != nil {
			continue
		}
		aliases = append(aliases, alias)
	}
	slices.Sort(aliases)
	return aliases, nil
}

// Remove deletes the configuration of alias, and with it the local copy of
// its secret.
func Remove(alias string) error {
	p, err := path(alias)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w %q", ErrUnknownHost, alias)
	}
	return err
}
