package runtime

// Tests for bundle.go: reading OCI runtime bundles' config.json, and
// writing one with runt spec. For how Go tests work, see "Tests" in doc.go.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeBundle writes config.json into a new bundle directory, starting from
// what WriteSpec writes and letting edit change it.
//
// The file is decoded into a map[string]any, rather than runtimeSpec, so
// edit can set any field, even ones runt doesn't know. Its values are then
// maps, slices, strings, and numbers as JSON has them, so getting at a
// nested object takes a type assertion, as in
// spec["process"].(map[string]any).
func writeBundle(t *testing.T, edit func(spec map[string]any)) string {
	t.Helper()
	dir := t.TempDir()
	if err := WriteSpec(dir, "rootfs"); err != nil {
		t.Fatal(err)
	}
	if err := WriteSpec(dir, "rootfs"); err == nil {
		t.Error("WriteSpec overwrote an existing config.json")
	}
	path := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	edit(spec)
	if data, err = json.Marshal(spec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestLoadBundle checks that LoadBundle reads the fields of config.json runt
// supports, starting from the file WriteSpec writes, and that command-line
// settings win over the file's.
func TestLoadBundle(t *testing.T) {
	dir := writeBundle(t, func(spec map[string]any) {
		process := spec["process"].(map[string]any)
		process["args"] = []string{"/bin/echo", "hi"}
		process["env"] = []string{"FOO=bar"}
		process["cwd"] = "/tmp"
		process["user"] = map[string]any{"uid": 1000, "gid": 1000, "additionalGids": []int{27}}
		spec["hostname"] = "bundled"
		linux := spec["linux"].(map[string]any)
		linux["uidMappings"] = []map[string]any{{"containerID": 0, "hostID": 200000, "size": 65536}}
		linux["gidMappings"] = []map[string]any{{"containerID": 0, "hostID": 300000, "size": 65536}}
		linux["resources"] = map[string]any{
			"memory": map[string]any{"limit": 64 << 20},
			"cpu":    map[string]any{"quota": 50000, "period": 100000},
			"pids":   map[string]any{"limit": 20},
		}
	})
	config := Config{Bundle: dir, Hostname: "from-flag"}
	if err := LoadBundle(&config); err != nil {
		t.Fatal(err)
	}
	if config.Rootfs != filepath.Join(dir, "rootfs") || !config.DirectRootfs || !config.ReadonlyRootfs {
		t.Errorf("root: %q direct=%v readonly=%v; want %q, direct, read-only", config.Rootfs, config.DirectRootfs, config.ReadonlyRootfs, filepath.Join(dir, "rootfs"))
	}
	if !slices.Equal(config.Command, []string{"/bin/echo", "hi"}) || !slices.Equal(config.Env, []string{"FOO=bar"}) || config.Cwd != "/tmp" {
		t.Errorf("process: %q env %q cwd %q", config.Command, config.Env, config.Cwd)
	}
	if config.User == nil || config.User.UID != 1000 || config.User.GID != 1000 || !slices.Equal(config.User.AdditionalGIDs, []uint32{27}) {
		t.Errorf("user = %+v; want 1000:1000 with group 27", config.User)
	}
	if !config.Tty {
		t.Error("terminal: true did not set Tty")
	}
	if config.Hostname != "from-flag" {
		t.Errorf("hostname = %q; want the command line's", config.Hostname)
	}
	if config.UIDs != (IDRange{200000, 65536}) || config.GIDs != (IDRange{300000, 65536}) {
		t.Errorf("ID ranges = %v, %v", config.UIDs, config.GIDs)
	}
	if config.Resources.Memory != 64<<20 || config.Resources.CPUs != 0.5 || config.Resources.PidsLimit != 20 {
		t.Errorf("resources = %+v", config.Resources)
	}
}

// TestLoadBundleRejects checks that LoadBundle refuses what runt can't do,
// rather than running the container differently from what config.json says.
func TestLoadBundleRejects(t *testing.T) {
	// A table whose cases are functions: each name maps to an edit that
	// makes config.json ask for something runt can't do.
	for name, edit := range map[string]func(spec map[string]any){
		"overlay mount": func(spec map[string]any) {
			spec["mounts"] = []map[string]any{{"destination": "/data", "type": "overlay", "source": "overlay"}}
		},
		"shared bind mount": func(spec map[string]any) {
			spec["mounts"] = []map[string]any{{"destination": "/data", "type": "bind", "source": "/srv", "options": []string{"rbind", "rshared"}}}
		},
		"bind mount without a source": func(spec map[string]any) {
			spec["mounts"] = []map[string]any{{"destination": "/data", "type": "bind"}}
		},
		"mount in /proc": func(spec map[string]any) {
			spec["mounts"] = []map[string]any{{"destination": "/proc/sys", "type": "bind", "source": "/srv"}}
		},
		"namespace path": func(spec map[string]any) {
			spec["linux"].(map[string]any)["namespaces"] = []map[string]any{{"type": "network", "path": "/proc/1/ns/net"}}
		},
		"several ID ranges": func(spec map[string]any) {
			spec["linux"].(map[string]any)["uidMappings"] = []map[string]any{
				{"containerID": 0, "hostID": 100000, "size": 1000},
				{"containerID": 1000, "hostID": 1000, "size": 1},
			}
		},
		"no args":      func(spec map[string]any) { spec["process"].(map[string]any)["args"] = []string{} },
		"relative cwd": func(spec map[string]any) { spec["process"].(map[string]any)["cwd"] = "tmp" },
		"version 2":    func(spec map[string]any) { spec["ociVersion"] = "2.0.0" },
	} {
		config := Config{Bundle: writeBundle(t, edit)}
		if err := LoadBundle(&config); err == nil {
			t.Errorf("%s: LoadBundle succeeded; want an error", name)
		}
	}
	// The standard mounts runc's own config.json lists are fine.
	config := Config{Bundle: writeBundle(t, func(map[string]any) {})}
	if err := LoadBundle(&config); err != nil {
		t.Errorf("LoadBundle of the spec as written: %v", err)
	}
	if !strings.HasPrefix(config.Rootfs, config.Bundle) {
		t.Errorf("rootfs %q is not inside bundle %q", config.Rootfs, config.Bundle)
	}
	if len(config.Mounts) != 0 {
		t.Errorf("the standard mounts became mounts of the container's own: %+v", config.Mounts)
	}
}

// TestLoadBundleMounts checks that bind and tmpfs mounts in config.json,
// after the standard ones, become the container's mounts, with a relative
// source taken from the bundle.
func TestLoadBundleMounts(t *testing.T) {
	dir := writeBundle(t, func(spec map[string]any) {
		// The standard mounts WriteSpec wrote, a []any once decoded, with
		// two more.
		spec["mounts"] = append(spec["mounts"].([]any),
			map[string]any{"destination": "/data", "type": "bind", "source": "data", "options": []string{"rbind", "ro", "rprivate"}},
			map[string]any{"destination": "/scratch", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "size=1m"}},
		)
	})
	config := Config{Bundle: dir}
	if err := LoadBundle(&config); err != nil {
		t.Fatal(err)
	}
	want := []Mount{
		{Type: MountBind, Source: filepath.Join(dir, "data"), Destination: "/data", Options: []string{"ro"}},
		{Type: MountTmpfs, Destination: "/scratch", Options: []string{"nosuid", "size=1048576"}},
	}
	// slices.EqualFunc compares two lists with a function of our own, here
	// one that compares two Mounts field by field.
	if !slices.EqualFunc(config.Mounts, want, func(a, b Mount) bool {
		return a.Type == b.Type && a.Source == b.Source && a.Destination == b.Destination && slices.Equal(a.Options, b.Options)
	}) {
		t.Errorf("mounts = %+v; want %+v", config.Mounts, want)
	}
}
