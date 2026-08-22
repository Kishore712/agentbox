package hostagent

import (
	"strings"
	"testing"
)

func TestSanitizeACLName_StripsUnsafeCharacters(t *testing.T) {
	got := sanitizeACLName("wl_8f2e:my-agent:k3n9q2")
	if strings.ContainsAny(got, ":") {
		t.Errorf("sanitized name still contains a colon: %q", got)
	}
	// hyphen is also not a safe Squid ACL-name character in all versions;
	// confirm it's replaced too, not just colons.
	if strings.Contains(got, "-") {
		t.Errorf("sanitized name still contains a hyphen: %q", got)
	}
}

func TestSanitizeACLName_DifferentIDsStayDistinct(t *testing.T) {
	a := sanitizeACLName("wl_1:agent-a:abc123")
	b := sanitizeACLName("wl_2:agent-b:abc123")
	if a == b {
		t.Errorf("two different instance IDs sanitized to the same ACL name: %q", a)
	}
}

func TestBuildSquidACLFragment_WithAllowlist(t *testing.T) {
	frag := BuildSquidACLFragment("wl_1:agent:abc", "172.16.0.2", []string{"api.openai.com", "pypi.org"})

	if !strings.Contains(frag, "src 172.16.0.2/32") {
		t.Errorf("missing source ACL for the guest IP, fragment:\n%s", frag)
	}
	if !strings.Contains(frag, "dstdomain api.openai.com pypi.org") {
		t.Errorf("missing destination allowlist ACL, fragment:\n%s", frag)
	}
	if !strings.Contains(frag, "http_access allow") {
		t.Errorf("missing an allow rule, fragment:\n%s", frag)
	}
	if !strings.Contains(frag, "http_access deny") {
		t.Errorf("missing a default-deny rule, fragment:\n%s", frag)
	}
	// The allow rule must come before the deny — Squid evaluates
	// http_access rules in order and stops at the first match.
	allowIdx := strings.Index(frag, "http_access allow")
	denyIdx := strings.Index(frag, "http_access deny")
	if allowIdx == -1 || denyIdx == -1 || allowIdx > denyIdx {
		t.Errorf("allow rule must precede the deny rule (Squid stops at first match), fragment:\n%s", frag)
	}
}

func TestBuildSquidACLFragment_EmptyAllowlistDeniesEverything(t *testing.T) {
	frag := BuildSquidACLFragment("wl_1:agent:abc", "172.16.0.2", nil)
	if strings.Contains(frag, "http_access allow") {
		t.Errorf("an empty allowlist must not produce any allow rule, fragment:\n%s", frag)
	}
	if !strings.Contains(frag, "http_access deny") {
		t.Errorf("expected a deny-everything rule for an empty allowlist, fragment:\n%s", frag)
	}
}

func TestBuildSquidACLFragment_DifferentInstancesDontShareACLNames(t *testing.T) {
	frag1 := BuildSquidACLFragment("wl_1:agent-a:abc", "172.16.0.2", []string{"x.com"})
	frag2 := BuildSquidACLFragment("wl_1:agent-b:xyz", "172.16.0.6", []string{"y.com"})
	// Extract the acl name tokens crudely and confirm they differ, so two
	// instances' fragments loaded together via `include conf.d/*.conf`
	// never collide.
	if strings.Contains(frag1, "src_"+sanitizeACLName("wl_1:agent-b:xyz")) {
		t.Error("fragment for one instance should not reference another instance's ACL name")
	}
	_ = frag2
}
