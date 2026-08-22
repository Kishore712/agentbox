package hostagent

import (
	"reflect"
	"testing"
)

func TestJailChrootRoot(t *testing.T) {
	got := jailChrootRoot("/srv/jailer", "/usr/local/bin/firecracker", "wl_1:agent:abc")
	want := "/srv/jailer/firecracker/wl_1:agent:abc/root"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestJailChrootRoot_UsesBinaryBasenameNotFullPath(t *testing.T) {
	// jailer's documented chroot convention keys on the exec-file's
	// basename, not its full path — confirm changing the binary's
	// directory doesn't change the chroot layout.
	a := jailChrootRoot("/srv/jailer", "/usr/local/bin/firecracker", "inst-1")
	b := jailChrootRoot("/srv/jailer", "/opt/bin/firecracker", "inst-1")
	if a != b {
		t.Errorf("chroot root should depend only on the binary's basename, got %q vs %q", a, b)
	}
}

func TestBuildJailerCommand(t *testing.T) {
	cfg := JailerConfig{
		JailerBinary: "/usr/local/bin/jailer", ChrootBaseDir: "/srv/jailer",
		UID: 123, GID: 456,
	}
	name, args := buildJailerCommand(cfg, "/usr/local/bin/firecracker", "inst-1", "api.sock")

	if name != "/usr/local/bin/jailer" {
		t.Errorf("name = %q, want the jailer binary path", name)
	}
	want := []string{
		"--id", "inst-1",
		"--exec-file", "/usr/local/bin/firecracker",
		"--uid", "123",
		"--gid", "456",
		"--chroot-base-dir", "/srv/jailer",
		"--",
		"--api-sock", "api.sock",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestBuildJailerCommand_SeparatorSplitsJailerArgsFromFirecrackerArgs(t *testing.T) {
	// Everything before "--" configures jailer itself; everything after is
	// passed through verbatim to firecracker. Confirm the split lands in
	// the right place regardless of config values.
	cfg := JailerConfig{JailerBinary: "jailer", ChrootBaseDir: "/x", UID: 1, GID: 1}
	_, args := buildJailerCommand(cfg, "firecracker", "inst-1", "api.sock")

	sepIdx := -1
	for i, a := range args {
		if a == "--" {
			sepIdx = i
			break
		}
	}
	if sepIdx == -1 {
		t.Fatal("expected a -- separator in the jailer args")
	}
	for _, jailerFlag := range []string{"--id", "--exec-file", "--uid", "--gid", "--chroot-base-dir"} {
		found := false
		for i, a := range args {
			if a == jailerFlag && i < sepIdx {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %s to appear before the -- separator", jailerFlag)
		}
	}
	if args[sepIdx+1] != "--api-sock" {
		t.Errorf("expected --api-sock to be the first firecracker-side arg after --, got %v", args[sepIdx+1:])
	}
}
