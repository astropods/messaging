//go:build !unix

package files

// Off unix the sidecar never runs (see fsstore_usage_other.go); these are
// build-only stubs so the package still compiles on other platforms. O_NOFOLLOW
// has no portable equivalent here, so no symlink protection is applied — which is
// acceptable only because this build target is never a real deployment.
const noFollowFlag = 0

// isSymlinkLoop is always false off unix: without O_NOFOLLOW there is no refused
// symlink to detect.
func isSymlinkLoop(error) bool { return false }
