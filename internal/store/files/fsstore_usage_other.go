//go:build !unix

package files

import "context"

// Usage is unsupported off unix (the sidecar only ever runs on linux; tests run
// on unix hosts). Return zero + ErrUnsupported so callers skip the capacity
// check rather than fail.
func (s *FSStore) Usage(_ context.Context) (Usage, error) {
	return Usage{}, ErrUnsupported
}
