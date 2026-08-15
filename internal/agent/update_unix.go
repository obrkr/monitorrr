//go:build !windows

package agent

import (
	"fmt"
	"os"
)

func defaultInstallPath() string { return "/usr/local/bin/monitorrr-agent" }

// swapBinary replaces the running executable.
//
// Unix allows renaming over a file that is currently executing: the running
// process keeps the old inode until it exits, so the swap is atomic and the
// current agent carries on unharmed until it chooses to stop.
func swapBinary(staged, target string) error {
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}

// cleanupOldBinary exists only for symmetry with Windows, which has to leave
// the previous binary behind until the next start.
func cleanupOldBinary() {}
