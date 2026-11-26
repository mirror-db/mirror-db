//go:build linux

package diskspace

import (
	"math"
	"syscall"
)

// systemProbe reports the bytes an unprivileged writer can still use on the
// filesystem holding path. Bavail excludes blocks reserved for root, which is
// the figure a cache write may actually consume.
func systemProbe(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	free := uint64(st.Bavail) * uint64(st.Bsize)
	if free > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(free), nil
}
