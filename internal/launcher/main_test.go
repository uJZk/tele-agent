package launcher

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.Cleanup(func(int) {
		if teleBin != "" {
			_ = os.RemoveAll(filepath.Dir(teleBin)) // built by buildTele
		}
	}))
}
