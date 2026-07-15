//go:build unix

package files

import (
	"context"

	"golang.org/x/sys/unix"
)

// Usage stats the filesystem backing the store's directory via statfs. On the
// shared /data volume this reflects the whole PVC (chat DB + files + agent
// outputs), which is exactly the capacity uploads compete for.
func (s *FSStore) Usage(_ context.Context) (Usage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(s.dir, &st); err != nil {
		return Usage{}, err
	}
	bsize := uint64(st.Bsize) //nolint:unconvert,gosec // Bsize is int64 on linux, uint32 on darwin
	total := st.Blocks * bsize
	avail := st.Bavail * bsize
	// Count root-reserved blocks (Blocks-Bfree) as used, matching `df`, so the
	// warning fires slightly early rather than after writes already start failing.
	used := (st.Blocks - st.Bfree) * bsize
	return Usage{TotalBytes: total, UsedBytes: used, AvailableBytes: avail}, nil
}
