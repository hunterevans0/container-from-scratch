package runtime

// Tests for mounts.go: parsing -v and --tmpfs, and checking a container's
// mounts against each other. For how Go tests work, see "Tests" in doc.go.

import (
	"path/filepath"
	"slices"
	"testing"
)

// sameMount reports whether two mounts are the same. Mount has a slice in
// it, so == can't compare two.
func sameMount(a, b Mount) bool {
	return a.Type == b.Type && a.Source == b.Source && a.Destination == b.Destination && slices.Equal(a.Options, b.Options)
}

// TestParseVolume checks each form -v takes, and that malformed ones are
// refused.
func TestParseVolume(t *testing.T) {
	// A host path is made absolute, which on Windows adds a drive letter,
	// so the paths wanted are made the same way.
	abs := func(p string) string {
		absolute, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return absolute
	}
	for _, test := range []struct {
		spec string
		want Mount
	}{
		{"/srv/data:/data", Mount{Type: MountBind, Source: abs("/srv/data"), Destination: "/data"}},
		{"/etc/hosts:/etc/hosts:ro", Mount{Type: MountBind, Source: abs("/etc/hosts"), Destination: "/etc/hosts", Options: []string{"ro"}}},
		{"./site:/usr/share/nginx/html:rw", Mount{Type: MountBind, Source: abs("site"), Destination: "/usr/share/nginx/html"}},
		{"..:/up", Mount{Type: MountBind, Source: abs(".."), Destination: "/up"}},
		{"pgdata:/var/lib/postgresql/data", Mount{Type: MountVolume, Source: "pgdata", Destination: "/var/lib/postgresql/data"}},
		{"cache:/cache:ro,nocopy", Mount{Type: MountVolume, Source: "cache", Destination: "/cache", Options: []string{"ro", "nocopy"}}},
	} {
		got, err := ParseVolume(test.spec)
		if err != nil {
			t.Errorf("ParseVolume(%q): %v", test.spec, err)
		} else if !sameMount(got, test.want) {
			t.Errorf("ParseVolume(%q) = %+v; want %+v", test.spec, got, test.want)
		}
	}
	for _, spec := range []string{
		"/data",                // an anonymous volume
		"data:relative",        // the destination isn't absolute
		"data:/",               // over the root
		"data:/proc/sys",       // runt's own
		"/srv:/sys",            // runt's own
		"data:/data:z",         // an SELinux label
		"/srv:/data:nocopy",    // nocopy is for volumes
		"-bad:/data",           // not a volume name
		"data dir:/data",       // not a volume name
		"a:b:c:d",              // too many parts
		"/srv:/data:ro:shared", // too many parts
	} {
		if got, err := ParseVolume(spec); err == nil {
			t.Errorf("ParseVolume(%q) = %+v; want an error", spec, got)
		}
	}
}

// TestParseTmpfs checks --tmpfs's defaults, options, and how later options
// win over earlier ones.
func TestParseTmpfs(t *testing.T) {
	for _, test := range []struct {
		spec string
		want []string
	}{
		{"/run", []string{"nosuid", "nodev", "noexec"}},
		{"/run:size=64m", []string{"nosuid", "nodev", "noexec", "size=67108864"}},
		{"/run:exec,mode=0755", []string{"nosuid", "nodev", "mode=755"}},
		{"/run:ro,mode=1777,size=1k,size=2k", []string{"ro", "nosuid", "nodev", "noexec", "size=2048", "mode=1777"}},
		{"/run:suid,dev,exec,rw", nil},
	} {
		got, err := ParseTmpfs(test.spec)
		want := Mount{Type: MountTmpfs, Destination: "/run", Options: test.want}
		if err != nil {
			t.Errorf("ParseTmpfs(%q): %v", test.spec, err)
		} else if !sameMount(got, want) {
			t.Errorf("ParseTmpfs(%q) = %+v; want %+v", test.spec, got, want)
		}
	}
	for _, spec := range []string{"run", "/", "/proc/x", "/run:size=", "/run:size=big", "/run:mode=999", "/run:mode=17777", "/run:uid=0", "/run:shared"} {
		if got, err := ParseTmpfs(spec); err == nil {
			t.Errorf("ParseTmpfs(%q) = %+v; want an error", spec, got)
		}
	}
}

// TestSortMounts checks that mounts are put in the order to make them in,
// parents first, and that two at one place are refused.
func TestSortMounts(t *testing.T) {
	mounts := []Mount{
		{Type: MountTmpfs, Destination: "/data/cache/"},
		{Type: MountBind, Source: "/srv/b", Destination: "/b"},
		{Type: MountVolume, Source: "data", Destination: "/data"},
		{Type: MountBind, Source: "/srv/a", Destination: "/a"},
	}
	sorted, err := sortMounts(mounts)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, mount := range sorted {
		order = append(order, mount.Destination)
	}
	// /b before /a, as given; and /data/cache/ cleaned.
	if want := []string{"/b", "/data", "/a", "/data/cache"}; !slices.Equal(order, want) {
		t.Errorf("order = %q; want %q", order, want)
	}
	if mounts[0].Destination != "/data/cache/" {
		t.Error("sortMounts changed the list it was given")
	}
	if _, err := sortMounts([]Mount{{Type: MountTmpfs, Destination: "/x"}, {Type: MountTmpfs, Destination: "/x/"}}); err == nil {
		t.Error("two mounts at /x were accepted")
	}
}

// TestMountCovers checks which paths are a mount's rather than the root
// filesystem's.
func TestMountCovers(t *testing.T) {
	mounts := []Mount{{Type: MountBind, Source: "/srv/etc", Destination: "/etc/"}, {Type: MountTmpfs, Destination: "/run"}}
	for p, want := range map[string]bool{
		"/etc/hosts":        true,
		"/etc":              true,
		"/run":              true,
		"/runtime":          false,
		"/var/run/foo":      false,
		"/etc2/resolv.conf": false,
	} {
		if got := mountCovers(mounts, p); got != want {
			t.Errorf("mountCovers(%q) = %v; want %v", p, got, want)
		}
	}
}
