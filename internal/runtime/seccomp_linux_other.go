//go:build linux && !amd64 && !arm64

package runtime

import (
	"fmt"
	goruntime "runtime"
)

// installSeccomp needs a table of syscall numbers for each architecture, and
// only amd64 and arm64 have one.
func installSeccomp() error {
	return fmt.Errorf("seccomp filter is not implemented for %s", goruntime.GOARCH)
}
