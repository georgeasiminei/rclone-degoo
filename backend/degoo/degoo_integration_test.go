package degoo_test

import (
	"testing"

	"github.com/rclone/rclone/backend/degoo"
	"github.com/rclone/rclone/fstest/fstests"
)

// TestIntegration runs the backend suite against a configured disposable vault.
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: "TestDegoo:",
		NilObject:  (*degoo.Object)(nil),
	})
}
