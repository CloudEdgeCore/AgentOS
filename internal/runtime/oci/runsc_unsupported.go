//go:build !linux

package oci

import (
	"fmt"
	"runtime"
)

// NewRunscExecutor exists on non-Linux platforms so the package still compiles,
// but the OCI/gVisor provider is Linux-only: containerd and runsc do not run
// on this platform and the worker must never fall back to an unsandboxed
// runtime.
func NewRunscExecutor(_ ...RunscOption) (Executor, error) {
	return nil, fmt.Errorf("OCI/gVisor provider requires Linux with containerd and runsc; current platform is %s/%s",
		runtime.GOOS, runtime.GOARCH)
}
