//go:build unix

package files

import (
	"errors"
	"syscall"
)

// noFollowFlag makes an open refuse to traverse a symlink at the final path
// component (returning ELOOP). Combined with the store's single-segment keys,
// this keeps every read and write confined to the store directory, so an agent
// can't plant "output.txt -> /etc/passwd" and have it served.
const noFollowFlag = syscall.O_NOFOLLOW

// isSymlinkLoop reports whether err is the "refused to follow a symlink" error
// from an O_NOFOLLOW open, so callers can treat it as "not a real file here".
func isSymlinkLoop(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
