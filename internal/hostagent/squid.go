package hostagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultSquidPort is where the host's Squid instance listens for explicit
// proxy connections from every instance on the host. Explicit proxying
// (HTTP_PROXY/HTTPS_PROXY in the guest, §4.6's init script) rather than
// transparent interception — Squid can enforce per-destination ACLs on the
// CONNECT method for HTTPS without decrypting anything, avoiding SSL-bump
// and the guest cert-trust problems that come with it.
const DefaultSquidPort = 3128

var aclNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// sanitizeACLName turns an instance_id (format {workload_id}:{name}:{random},
// §4.1) into a Squid ACL-name-safe token — Squid ACL names can't contain
// colons or other punctuation the ID format uses.
func sanitizeACLName(instanceID string) string {
	return aclNameSanitizer.ReplaceAllString(instanceID, "_")
}

// BuildSquidACLFragment generates the per-instance Squid config fragment
// (§4.8): an ACL matching this instance's guest IP as source, an ACL
// listing its allowed destination domains, and access rules enforcing the
// allowlist for that source while denying everything else. Pure and fully
// unit-testable — no real Squid needed to verify the generated config is
// correct, only to verify Squid actually accepts and applies it (untestable
// here, real validation is a GCP-phase concern).
func BuildSquidACLFragment(instanceID, guestIP string, allowlist []string) string {
	name := sanitizeACLName(instanceID)
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated for instance %s — do not edit by hand\n", instanceID)
	fmt.Fprintf(&b, "acl src_%s src %s/32\n", name, guestIP)
	if len(allowlist) == 0 {
		fmt.Fprintf(&b, "http_access deny src_%s\n", name)
		return b.String()
	}
	fmt.Fprintf(&b, "acl dst_%s dstdomain %s\n", name, strings.Join(allowlist, " "))
	fmt.Fprintf(&b, "http_access allow src_%s dst_%s\n", name, name)
	fmt.Fprintf(&b, "http_access deny src_%s\n", name)
	return b.String()
}

// SquidManager writes per-instance ACL fragments into Squid's conf.d-style
// include directory and reloads Squid to apply them (§4.8). The fragment
// generation above is genuinely unit-tested; actually applying and
// reloading requires a real running Squid process, not available on this
// dev machine — exercised only in the GCP validation phase.
type SquidManager struct {
	ConfDir string // e.g. "/etc/squid/conf.d", included from squid.conf via `include conf.d/*.conf`
}

func (s *SquidManager) fragmentPath(instanceID string) string {
	return filepath.Join(s.ConfDir, sanitizeACLName(instanceID)+".conf")
}

// ApplyACL writes (or overwrites) instanceID's fragment and reloads Squid.
func (s *SquidManager) ApplyACL(ctx context.Context, instanceID, guestIP string, allowlist []string) error {
	if err := os.MkdirAll(s.ConfDir, 0o755); err != nil {
		return fmt.Errorf("create squid conf.d: %w", err)
	}
	frag := BuildSquidACLFragment(instanceID, guestIP, allowlist)
	if err := os.WriteFile(s.fragmentPath(instanceID), []byte(frag), 0o644); err != nil {
		return fmt.Errorf("write squid fragment: %w", err)
	}
	return s.reload(ctx)
}

// RemoveACL deletes instanceID's fragment and reloads Squid. Idempotent —
// removing a fragment that's already gone is not an error, matching
// TeardownNetwork's idempotency requirement (§4.3).
func (s *SquidManager) RemoveACL(ctx context.Context, instanceID string) error {
	if err := os.Remove(s.fragmentPath(instanceID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove squid fragment: %w", err)
	}
	return s.reload(ctx)
}

func (s *SquidManager) reload(ctx context.Context) error {
	return runCmd(ctx, "squid", "-k", "reconfigure")
}
