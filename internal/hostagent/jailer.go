package hostagent

import (
	"path/filepath"
	"strconv"
)

// JailerConfig enables the Firecracker Jailer (§4.6/Phase 6 hardening):
// chroot + cgroup + seccomp + privilege drop around the firecracker
// process, instead of running it bare.
//
// Honest caveat: the argument construction here matches Firecracker's
// documented jailer CLI, and is unit-tested against that documentation —
// but the exact chroot path semantics (whether firecracker's own CWD lands
// at the chroot root, what leading-slash convention it expects for
// --api-sock, cgroup v1 vs v2 handling) are this author's best-faith
// reading, not verified against a real jailer binary, which doesn't exist
// on this dev machine. This is the first thing to confirm empirically in
// the GCP validation phase, before trusting it in anything beyond a
// prototype.
type JailerConfig struct {
	Enabled       bool
	JailerBinary  string // e.g. "/usr/local/bin/jailer"
	ChrootBaseDir string // e.g. "/srv/jailer"
	UID           int
	GID           int
}

// jailChrootRoot returns the host-visible path to instanceID's chroot root
// — jailer's documented convention is
// {chroot-base-dir}/{exec-file-basename}/{id}/root/.
func jailChrootRoot(chrootBaseDir, firecrackerBinary, instanceID string) string {
	return filepath.Join(chrootBaseDir, filepath.Base(firecrackerBinary), instanceID, "root")
}

// buildJailerCommand constructs the jailer CLI invocation. Pure and fully
// unit-testable — verifying the exact args match Firecracker's documented
// jailer usage doesn't need a real jailer binary present, only real
// *execution* does.
func buildJailerCommand(cfg JailerConfig, firecrackerBinary, instanceID, apiSockRelPath string) (name string, args []string) {
	args = []string{
		"--id", instanceID,
		"--exec-file", firecrackerBinary,
		"--uid", strconv.Itoa(cfg.UID),
		"--gid", strconv.Itoa(cfg.GID),
		"--chroot-base-dir", cfg.ChrootBaseDir,
		"--",
		"--api-sock", apiSockRelPath,
	}
	return cfg.JailerBinary, args
}
