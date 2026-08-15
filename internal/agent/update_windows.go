//go:build windows

package agent

import (
	"fmt"
	"os"
)

func defaultInstallPath() string { return `C:\Program Files\monitorrr\monitorrr-agent.exe` }

// swapBinary replaces the running executable.
//
// Windows refuses to delete or overwrite a running image, but it does allow
// renaming one. Moving ourselves aside frees the name for the replacement; the
// leftover is cleaned up on the next start, once nothing has it open.
func swapBinary(staged, target string) error {
	old := target + ".old"
	os.Remove(old) // a leftover from a previous update, if any

	if err := os.Rename(target, old); err != nil {
		return fmt.Errorf("move the running binary aside: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		// Put it back rather than leaving the machine with no agent at all.
		if restoreErr := os.Rename(old, target); restoreErr != nil {
			return fmt.Errorf("install failed (%w) and the original could not be restored: %v", err, restoreErr)
		}
		return fmt.Errorf("install replacement: %w", err)
	}
	return nil
}

// cleanupOldBinary removes the previous version left behind by an update. Safe
// to call at any time: it does nothing if there is nothing to remove.
func cleanupOldBinary() {
	if exe, err := ownExecutable(); err == nil {
		os.Remove(exe + ".old")
	}
}
