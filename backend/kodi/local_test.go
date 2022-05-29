// Test Local filesystem interface
package kodi_test

import (
	"testing"

	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fstest/fstests"
)

// TestIntegration runs integration tests against the remote
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: "",
		NilObject:  (*local.Object)(nil),
	})
}
