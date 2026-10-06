//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// applyAppArmorProfile asks the kernel to switch to profile at the next exec,
// so the profile confines the container's command but not this init process,
// which still has mounts to make. The request belongs to the calling thread
// and is inherited by a child forked from it, so the caller must hold the
// thread with LockOSThread until the command has been started.
//
// The profile must already be loaded into the kernel (see `make apparmor`).
func applyAppArmorProfile(profile string) error {
	enabled, err := os.ReadFile("/sys/module/apparmor/parameters/enabled")
	if err != nil || strings.TrimSpace(string(enabled)) != "Y" {
		return errors.New("AppArmor is not enabled on this kernel; on WSL2, see scripts/enable-apparmor-wsl.sh")
	}

	// Older kernels only have the attribute file that all security modules
	// share.
	file, err := os.OpenFile("/proc/thread-self/attr/apparmor/exec", os.O_WRONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		file, err = os.OpenFile("/proc/thread-self/attr/exec", os.O_WRONLY, 0)
	}
	if err != nil {
		return fmt.Errorf("apply AppArmor profile %q: %w", profile, err)
	}
	defer file.Close()

	_, err = file.WriteString("exec " + profile)
	if errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("AppArmor profile %q is not loaded; run `make apparmor`", profile)
	}
	if err != nil {
		return fmt.Errorf("apply AppArmor profile %q: %w", profile, err)
	}
	return nil
}
