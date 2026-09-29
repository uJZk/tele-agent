package launcher

import (
	"testing"

	"github.com/ujzk/tele-agent/internal/proto"
)

func testTarget(t *testing.T) *proto.TargetInfo {
	return &proto.TargetInfo{
		Hostname: "target", User: "bob", Home: t.TempDir(), Shell: "/bin/sh",
		LoginPath: "/usr/local/bin:/usr/bin:/bin",
	}
}
