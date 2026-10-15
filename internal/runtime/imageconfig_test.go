package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestApplyImageConfig checks how an image's defaults combine with the
// command line, as in docker run.
func TestApplyImageConfig(t *testing.T) {
	image := &ociImage{}
	defaults := &image.config.Config
	defaults.Entrypoint = []string{"/entry"}
	defaults.Cmd = []string{"default", "args"}
	defaults.Env = []string{"PATH=/image/bin", "A=1", "B=2"}
	defaults.WorkingDir = "/app"
	defaults.User = "app"
	empty := ""
	other := "/other"

	for _, test := range []struct {
		name   string
		config Config
		want   Config
	}{
		{"defaults", Config{},
			Config{Command: []string{"/entry", "default", "args"}, Env: []string{"PATH=/image/bin", "A=1", "B=2"}, Cwd: "/app", UserSpec: "app"}},
		{"arguments replace CMD", Config{Command: []string{"mine"}},
			Config{Command: []string{"/entry", "mine"}, Env: []string{"PATH=/image/bin", "A=1", "B=2"}, Cwd: "/app", UserSpec: "app"}},
		{"--entrypoint drops CMD", Config{Entrypoint: &other},
			Config{Command: []string{"/other"}, Env: []string{"PATH=/image/bin", "A=1", "B=2"}, Cwd: "/app", UserSpec: "app"}},
		{"--entrypoint '' with arguments", Config{Entrypoint: &empty, Command: []string{"sh"}},
			Config{Command: []string{"sh"}, Env: []string{"PATH=/image/bin", "A=1", "B=2"}, Cwd: "/app", UserSpec: "app"}},
		{"-e, -w, -u, -t", Config{SetEnv: []string{"B=3", "C=4"}, Cwd: "/work", UserSpec: "0:0", Tty: true},
			Config{Command: []string{"/entry", "default", "args"}, Env: []string{"PATH=/image/bin", "A=1", "B=3", "TERM=xterm", "C=4"}, Cwd: "/work", UserSpec: "0:0", Tty: true}},
	} {
		config := test.config
		if err := applyImageConfig(&config, image); err != nil {
			t.Errorf("%s: %v", test.name, err)
			continue
		}
		if !slices.Equal(config.Command, test.want.Command) || !slices.Equal(config.Env, test.want.Env) ||
			config.Cwd != test.want.Cwd || config.UserSpec != test.want.UserSpec {
			t.Errorf("%s: got command %q env %q cwd %q user %q; want %q %q %q %q", test.name,
				config.Command, config.Env, config.Cwd, config.UserSpec,
				test.want.Command, test.want.Env, test.want.Cwd, test.want.UserSpec)
		}
	}

	// An image with nothing set runs as root in /, with a PATH, and needs a
	// command.
	bare := &ociImage{}
	config := Config{Command: []string{"ls"}}
	if err := applyImageConfig(&config, bare); err != nil {
		t.Fatal(err)
	}
	if config.Cwd != "/" || config.UserSpec != "" || !slices.Equal(config.Env, []string{defaultPath}) {
		t.Errorf("bare image: cwd %q user %q env %q", config.Cwd, config.UserSpec, config.Env)
	}
	if err := applyImageConfig(&Config{}, bare); err == nil {
		t.Error("an image without a command ran without one given")
	}
	if err := applyImageConfig(&Config{Command: []string{"ls"}, Cwd: "relative"}, bare); err == nil {
		t.Error("a relative working directory was accepted")
	}
}

// TestResolveUser checks user specifications against an /etc/passwd and
// /etc/group.
func TestResolveUser(t *testing.T) {
	dir := t.TempDir()
	passwdPath := filepath.Join(dir, "passwd")
	groupPath := filepath.Join(dir, "group")
	os.WriteFile(passwdPath, []byte(`root:x:0:0:root:/root:/bin/sh
# a comment
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
app:x:1000:1000::/home/app:/bin/sh
broken line
nohome:x:1001:1001:::/bin/sh
`), 0o644)
	os.WriteFile(groupPath, []byte(`root:x:0:
wheel:x:10:root,app
app:x:1000:
audio:x:29:app,other
`), 0o644)
	passwd, err := readPasswd(passwdPath)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := readGroup(groupPath)
	if err != nil {
		t.Fatal(err)
	}

	for spec, want := range map[string]struct {
		user User
		home string
	}{
		"":           {User{UID: 0, GID: 0, AdditionalGIDs: []uint32{10}}, "/root"},
		"root":       {User{UID: 0, GID: 0, AdditionalGIDs: []uint32{10}}, "/root"},
		"app":        {User{UID: 1000, GID: 1000, AdditionalGIDs: []uint32{10, 29}}, "/home/app"},
		"1000":       {User{UID: 1000, GID: 1000, AdditionalGIDs: []uint32{10, 29}}, "/home/app"},
		"app:audio":  {User{UID: 1000, GID: 29, AdditionalGIDs: []uint32{10}}, "/home/app"},
		"daemon:0":   {User{UID: 1, GID: 0}, "/usr/sbin"},
		"5000":       {User{UID: 5000, GID: 0}, "/"},
		"5000:5000":  {User{UID: 5000, GID: 5000}, "/"},
		"nohome":     {User{UID: 1001, GID: 1001}, "/"},
		"1000:wheel": {User{UID: 1000, GID: 10, AdditionalGIDs: []uint32{29}}, "/home/app"},
	} {
		user, home, err := resolveUser(spec, passwd, groups)
		if err != nil {
			t.Errorf("resolveUser(%q): %v", spec, err)
			continue
		}
		if user.UID != want.user.UID || user.GID != want.user.GID || !slices.Equal(user.AdditionalGIDs, want.user.AdditionalGIDs) || home != want.home {
			t.Errorf("resolveUser(%q) = %+v, %q; want %+v, %q", spec, user, home, want.user, want.home)
		}
	}
	for _, spec := range []string{"nobody", "app:nogroup", "-1"} {
		if user, _, err := resolveUser(spec, passwd, groups); err == nil {
			t.Errorf("resolveUser(%q) = %+v; want an error", spec, user)
		}
	}
	if home := homeOf(1, passwd); home != "/usr/sbin" {
		t.Errorf("homeOf(1) = %q", home)
	}

	// No /etc/passwd at all: numeric IDs still work.
	if passwd, err := readPasswd(filepath.Join(dir, "missing")); err != nil || passwd != nil {
		t.Errorf("reading a missing /etc/passwd: %v, %v", passwd, err)
	}
	// Something other than a regular file is refused.
	if _, err := readPasswd(dir); err == nil {
		t.Error("read a directory as /etc/passwd")
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2", "A=3"}, []string{"A=x", "C=y", "B="})
	if want := []string{"A=x", "B=", "A=x", "C=y"}; !slices.Equal(got, want) {
		t.Errorf("mergeEnv = %q; want %q", got, want)
	}
}
