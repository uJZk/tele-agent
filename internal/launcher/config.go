package launcher

import (
	"github.com/ujzk/tele-agent/internal/endpoint"
	"github.com/ujzk/tele-agent/internal/hostcfg"
)

// loadHost reads the configuration of alias and returns its endpoint with
// its credentials; token is set only for a unix endpoint.
// endpointOverride, if set, replaces the configured endpoint.
func loadHost(alias, endpointOverride string) (endpoint.Endpoint, []byte, error) {
	h, err := hostcfg.Load(alias)
	if err != nil {
		return endpoint.Endpoint{}, nil, err
	}
	return h.Resolve(endpointOverride)
}

// loadHostAll is loadHost with the host's alternate endpoints too.
func loadHostAll(alias, endpointOverride string) ([]endpoint.Endpoint, []byte, error) {
	h, err := hostcfg.Load(alias)
	if err != nil {
		return nil, nil, err
	}
	return h.ResolveAll(endpointOverride)
}
