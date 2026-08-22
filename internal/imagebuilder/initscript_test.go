package imagebuilder

import (
	"strings"
	"testing"
)

func TestBuildInitScript_SimpleEntrypoint(t *testing.T) {
	script, err := BuildInitScript([]string{"python3", "app.py"})
	if err != nil {
		t.Fatalf("BuildInitScript: %v", err)
	}
	if !strings.HasPrefix(script, "#!/bin/sh") {
		t.Errorf("script must start with a shebang, got: %s", script[:min(40, len(script))])
	}
	if !strings.Contains(script, "mount -t ext4 /dev/vdb "+GuestHomeDir) {
		t.Errorf("expected the home volume mount, script:\n%s", script)
	}
	if !strings.Contains(script, "export HOME="+GuestHomeDir) {
		t.Errorf("expected HOME to be set to the platform convention, script:\n%s", script)
	}
	if !strings.Contains(script, "exec 'python3' 'app.py'") {
		t.Errorf("expected the entrypoint to be exec'd, script:\n%s", script)
	}
}

func TestBuildInitScript_EmptyEntrypointFails(t *testing.T) {
	_, err := BuildInitScript(nil)
	if err == nil {
		t.Fatal("expected an error for an empty entrypoint")
	}
}

func TestBuildInitScript_ShellQuotingEscapesEmbeddedQuotes(t *testing.T) {
	script, err := BuildInitScript([]string{"sh", "-c", "echo it's here"})
	if err != nil {
		t.Fatal(err)
	}
	// The embedded single quote must be safely escaped, not break the script.
	if !strings.Contains(script, `'echo it'\''s here'`) {
		t.Errorf("expected safely escaped embedded quote, script:\n%s", script)
	}
}

func TestBuildInitScript_ConfiguresProxyFromCmdline(t *testing.T) {
	script, err := BuildInitScript([]string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "/proc/cmdline") {
		t.Errorf("expected the script to read the proxy address from /proc/cmdline, script:\n%s", script)
	}
	if !strings.Contains(script, "platform\\.squid_proxy=") {
		t.Errorf("expected the script to parse platform.squid_proxy=, script:\n%s", script)
	}
	for _, want := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if !strings.Contains(script, want) {
			t.Errorf("expected the script to export %s, script:\n%s", want, script)
		}
	}
	// The proxy setup must happen before exec, not after (an exec replaces
	// the process — anything after it never runs).
	proxyIdx := strings.Index(script, "SQUID_PROXY=")
	execIdx := strings.Index(script, "exec ")
	if proxyIdx == -1 || execIdx == -1 || proxyIdx > execIdx {
		t.Errorf("proxy setup must come before exec, script:\n%s", script)
	}
}

func TestBuildInitScript_NetworkingNotGuestSideResponsibility(t *testing.T) {
	// Regression guard for a design decision: the kernel's own ip= boot
	// argument handles networking (§4.3) — init must NOT contain any
	// network configuration commands (ip addr, dhclient, etc.).
	script, err := BuildInitScript([]string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"ip addr", "ip link", "dhclient", "ifconfig"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("init script should not configure networking (kernel's ip= boot arg already does this), found %q", forbidden)
		}
	}
}
