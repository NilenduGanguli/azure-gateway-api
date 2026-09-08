//go:build !linux && !darwin

package store

import "errors"

// diskUsage is unsupported outside Linux and Darwin; callers treat the error as "unknown usage"
// and skip high-water-mark eviction rather than failing.
func diskUsage(string) (free, total uint64, err error) {
	return 0, 0, errors.New("store: disk usage unsupported on this platform")
}

// networkFS cannot be determined on this platform.
func networkFS(string) (bool, string) { return false, "" }
