package launcher

import (
	"context"

	"github.com/ujzk/tele-agent/internal/client"
	"github.com/ujzk/tele-agent/internal/endpoint"
	"github.com/ujzk/tele-agent/internal/resume"
)

// remoteSession is an established session with tele server.
type remoteSession = client.Session

// connect establishes a session with the server at ep (docs/cli.md "启动流程"
// steps 1 and 2).
func connect(ctx context.Context, ep endpoint.Endpoint, token []byte, cfg resume.Config) (*remoteSession, error) {
	return client.Connect(ctx, ep, token, cfg)
}
