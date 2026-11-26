//go:build !linux

package diskspace

import "errors"

var errProbeUnsupported = errors.New("free-space probe is not implemented on this platform")

// systemProbe degrades to an error, which Check logs once and treats as
// sufficient space. A non-Linux build keeps explicit capacity limits only.
func systemProbe(string) (int64, error) { return 0, errProbeUnsupported }
