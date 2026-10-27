//go:build integration && linux

package integration

// Storage tests, as feature checks (see the top of features_test.go): bind
// mounts and volumes (-v), and tmpfs mounts (--tmpfs). For how to read these
// tests, see the top of isolation_test.go.

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// containerRootDir makes a directory on the host owned by container root,
// as the rootfs is, so a container can write to it, and returns it. Files
// inside are given as name and content pairs.
func containerRootDir(t *testing.T, r *runtTester, files ...string) string {
	t.Helper()
	var rootStat syscall.Stat_t
	if err := syscall.Stat(r.rootfs, &rootStat); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chown(dir, int(rootStat.Uid), int(rootStat.Gid)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(files); i += 2 {
		writeFile(t, filepath.Join(dir, files[i]), files[i+1], 0o644)
	}
	return dir
}

// mountLine returns the line of the container's /proc/mounts for the mount
// at path, from output that has them all, or "" if there is none. Each line
// is what is mounted, where, the filesystem type, and its options.
func mountLine(output, path string) string {
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) >= 4 && fields[1] == path {
			return line
		}
	}
	return ""
}

// removeVolumeAtEnd removes volume name, if there is one, now and when the
// test ends. Called before the test makes its containers, it runs after
// they are removed (cleanups run last first).
func (r *runtTester) removeVolumeAtEnd(name string) {
	r.t.Helper()
	r.runt("volume", "rm", name)
	r.t.Cleanup(func() {
		if output, err := r.runt("volume", "rm", name); err != nil && !strings.Contains(output, "no such volume") {
			r.t.Errorf("runt volume rm %s: %v\n%s", name, err, output)
		}
	})
}

// TestFeatureBindMounts checks bind mounts: -v /host/path:/container/path.
func TestFeatureBindMounts(t *testing.T) {
	explain(t, "Bind mounts: a file or directory on the host, in the container (-v /host/path:/path)")

	t.Run("HostDirectoryIsShared", func(t *testing.T) {
		explain(t, "the container reads the host's files, and what it writes there the host sees")
		r := newRuntTester(t)
		dir := containerRootDir(t, r, "greeting", "from-the-host-"+nonce+"\n")
		output, err := r.runOnce("runt-feature-bind", "--rootfs", r.rootfs, "-v", dir+":/data", "--", "/bin/sh", "-c",
			"cat /data/greeting && echo from-the-container > /data/reply")
		mustSucceed(t, "runt run -v", output, err, "from-the-host-"+nonce)
		if data, err := os.ReadFile(filepath.Join(dir, "reply")); err != nil || string(data) != "from-the-container\n" {
			t.Errorf("the host doesn't see what the container wrote: %v, %q", err, data)
		}
		var stat syscall.Stat_t
		if syscall.Stat(filepath.Join(dir, "reply"), &stat) == nil {
			observe("the container's file on the host: %s/reply, owned by host uid %d", dir, stat.Uid)
		}
	})

	t.Run("ReadOnly", func(t *testing.T) {
		explain(t, ":ro mounts it read-only")
		r := newRuntTester(t)
		dir := containerRootDir(t, r)
		output, _ := r.runOnce("runt-feature-bind-ro", "--rootfs", r.rootfs, "-v", dir+":/data:ro", "--", "/bin/sh", "-c",
			"grep ' /data ' /proc/mounts; touch /data/written")
		line := mountLine(output, "/data")
		if !strings.HasPrefix(strings.Fields(line + " - - - -")[3], "ro") {
			t.Errorf("/data isn't mounted read-only:\n%s", output)
		}
		if !strings.Contains(output, "Read-only file system") {
			t.Errorf("writing to /data didn't fail as read-only:\n%s", output)
		}
		if _, err := os.Stat(filepath.Join(dir, "written")); err == nil {
			t.Error("the container wrote to a read-only bind mount")
		}
		observe("%s", line)
	})

	t.Run("SingleFileOverRuntsOwn", func(t *testing.T) {
		explain(t, "a single file can be mounted, and one at /etc/hosts replaces runt's")
		r := newRuntTester(t)
		dir := containerRootDir(t, r, "hosts", "10.1.2.3\tcustom-"+nonce+"\n")
		output, err := r.runOnce("runt-feature-bind-file", "--rootfs", r.rootfs, "-v", filepath.Join(dir, "hosts")+":/etc/hosts:ro", "--", "/bin/cat", "/etc/hosts")
		mustSucceed(t, "runt run -v FILE:/etc/hosts", output, err, "custom-"+nonce)
		if strings.Contains(output, "localhost") {
			t.Errorf("runt's own /etc/hosts was mounted over the container's:\n%s", output)
		}
	})

	t.Run("SourceMustExist", func(t *testing.T) {
		explain(t, "a host path that doesn't exist is an error, rather than made empty")
		r := newRuntTester(t)
		missing := "/runt-feature-missing-" + nonce
		output, err := r.runOnce("runt-feature-bind-missing", "--rootfs", r.rootfs, "-v", missing+":/data", "--", "/bin/true")
		mustFail(t, "runt run -v with a missing source", output, err, "does not exist")
		if _, err := os.Stat(missing); err == nil {
			os.Remove(missing)
			t.Errorf("runt made %s", missing)
		}
	})

	t.Run("StagingIsGoneOnceRunning", func(t *testing.T) {
		explain(t, "the host's staging mounts are gone once the container is running; its own copies stay")
		r := newRuntTester(t)
		dir := containerRootDir(t, r, "greeting", "hello\n")
		id := r.startWith("runt-feature-bind-staged", []string{"--rootfs", r.rootfs, "-v", dir + ":/data"}, "/bin/sleep", "600")
		if _, err := os.Stat(filepath.Join("/run/runt-mounts", id)); err == nil {
			t.Errorf("/run/runt-mounts/%s is still there", id)
		}
		mounts, err := os.ReadFile("/proc/self/mounts")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(mounts), "/run/runt-mounts/"+id) {
			t.Errorf("a staging mount is still mounted on the host")
		}
		output, err := r.exec("runt-feature-bind-staged", "cat /data/greeting")
		mustSucceed(t, "reading the bind mount after staging is gone", output, err, "hello")
	})

	t.Run("SymlinksStayInTheContainer", func(t *testing.T) {
		explain(t, "a mount point under a symlink in the image is made where the container sees it, not on the host")
		r := newRuntTester(t)
		// The image has /escape, a link to a directory on the host, by its
		// host path. Followed on the host, as the kernel would follow it
		// before pivot_root, it would lead out of the container.
		hostDir := t.TempDir()
		b := newLayoutBuilder(t)
		base, baseDiff := b.addBaseLayer(r)
		var layer bytes.Buffer
		archive := tar.NewWriter(&layer)
		if err := archive.WriteHeader(&tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: hostDir, Mode: 0o777}); err != nil {
			t.Fatal(err)
		}
		archive.Close()
		top, topDiff := b.tarLayer(layer.Bytes())
		b.writeImage("symlink", nil, []map[string]any{base, top}, []string{baseDiff, topDiff})
		r.removeUnpackedAtEnd(top)

		output, err := r.runOnce("runt-feature-bind-symlink", "--image", b.dir+":symlink", "--tmpfs", "/escape/sub", "--", "/bin/sh", "-c", "cat /proc/mounts")
		mustSucceed(t, "runt run --tmpfs under the link", output, err)
		if _, err := os.Stat(filepath.Join(hostDir, "sub")); err == nil {
			t.Errorf("the mount point was made on the host, in %s", hostDir)
		}
		if line := mountLine(output, filepath.Join(hostDir, "sub")); line == "" {
			t.Errorf("no mount at %s/sub in the container:\n%s", hostDir, output)
		} else {
			observe("in the container: %s", line)
		}
	})

	t.Run("BundleMounts", func(t *testing.T) {
		explain(t, "a bundle's config.json can list bind and tmpfs mounts, which stay writable under its read-only root")
		r := newRuntTester(t)
		dir := containerRootDir(t, r)
		bundle, spec := writeFeatureBundle(t, r)
		spec["mounts"] = append(spec["mounts"].([]any),
			map[string]any{"destination": "/mnt", "type": "bind", "source": dir, "options": []string{"rbind", "rw"}},
			map[string]any{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "nodev", "mode=1777"}},
		)
		spec["process"].(map[string]any)["args"] = []string{"/bin/sh", "-c", "touch /mnt/from-bundle /tmp/scratch && echo mounts-writable; touch /root-is-read-only"}
		saveBundle(t, bundle, spec)
		output, _ := r.runOnce("runt-feature-bind-bundle", "--bundle", bundle)
		for _, want := range []string{"mounts-writable", "Read-only file system"} {
			if !strings.Contains(output, want) {
				t.Errorf("output is missing %q:\n%s", want, output)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "from-bundle")); err != nil {
			t.Errorf("the bundle's bind mount didn't reach the host: %v", err)
		}
	})
}

// TestFeatureVolumes checks named volumes: -v NAME:/path, and runt volume.
func TestFeatureVolumes(t *testing.T) {
	explain(t, "Named volumes: directories runt keeps, which outlive the containers using them (-v NAME:/path)")

	t.Run("OutlivesItsContainer", func(t *testing.T) {
		explain(t, "what one container writes to a volume, the next one reads, after the first is removed")
		r := newRuntTester(t)
		volume := "runt-feature-vol-" + nonce
		r.removeVolumeAtEnd(volume)
		output, err := r.runOnce("runt-feature-vol-a", "--rootfs", r.rootfs, "-v", volume+":/data", "--", "/bin/sh", "-c", "echo kept-"+nonce+" > /data/file")
		mustSucceed(t, "writing to the volume", output, err)
		output, err = r.runOnce("runt-feature-vol-b", "--rootfs", r.rootfs, "-v", volume+":/data", "--", "/bin/cat", "/data/file")
		mustSucceed(t, "reading it from a new container", output, err, "kept-"+nonce)
		observe("kept in /var/lib/runt/volumes/%s/_data", volume)
	})

	t.Run("CreateLsRm", func(t *testing.T) {
		explain(t, "runt volume create, ls, and rm; a new volume belongs to container root")
		r := newRuntTester(t)
		volume := "runt-feature-vol-made-" + nonce
		r.removeVolumeAtEnd(volume)
		output, err := r.runt("volume", "create", volume)
		mustSucceed(t, "runt volume create", output, err, volume)
		data := filepath.Join("/var/lib/runt/volumes", volume, "_data")
		output, err = r.runt("volume", "ls")
		mustSucceed(t, "runt volume ls", output, err, volume, data)
		var stat, rootStat syscall.Stat_t
		if err := syscall.Stat(data, &stat); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Stat(r.rootfs, &rootStat); err != nil {
			t.Fatal(err)
		}
		if stat.Uid != rootStat.Uid || stat.Gid != rootStat.Gid {
			t.Errorf("%s is owned by %d:%d; want container root, %d:%d (if you made the rootfs with USERNS_USER, this is expected)", data, stat.Uid, stat.Gid, rootStat.Uid, rootStat.Gid)
		}
		output, err = r.runt("volume", "rm", volume)
		mustSucceed(t, "runt volume rm", output, err)
		if output, _ := r.runt("volume", "ls", "-q"); strings.Contains(output, volume) {
			t.Errorf("runt volume ls still lists %s:\n%s", volume, output)
		}
		if _, err := os.Stat(data); err == nil {
			t.Errorf("runt volume rm left %s behind", data)
		}
	})

	t.Run("InUseUntilContainerRemoved", func(t *testing.T) {
		explain(t, "a volume can't be removed while a container, even a stopped one, uses it")
		r := newRuntTester(t)
		volume := "runt-feature-vol-used-" + nonce
		r.removeVolumeAtEnd(volume)
		name := "runt-feature-vol-user"
		r.removeAtEnd(name)
		output, err := r.runt("run", "--name", name, "--rootfs", r.rootfs, "-v", volume+":/data", "--", "/bin/true")
		mustSucceed(t, "runt run -v", output, err)
		output, err = r.runt("volume", "rm", volume)
		mustFail(t, "runt volume rm of a volume a stopped container uses", output, err, "in use by container")
		if output, err := r.runt("rm", name); err != nil {
			t.Fatalf("runt rm: %v\n%s", err, output)
		}
		output, err = r.runt("volume", "rm", volume)
		mustSucceed(t, "runt volume rm once the container is removed", output, err)
	})

	t.Run("NewVolumeFilledFromImage", func(t *testing.T) {
		explain(t, "an empty volume starts with the image's files at its mount point, unless :nocopy")
		r := newRuntTester(t)
		volume, empty := "runt-feature-vol-filled-"+nonce, "runt-feature-vol-nocopy-"+nonce
		r.removeVolumeAtEnd(volume)
		r.removeVolumeAtEnd(empty)
		output, err := r.runOnce("runt-feature-vol-filled", "--rootfs", r.rootfs, "-v", volume+":/etc/skel", "--", "/bin/ls", "-A", "/etc/skel")
		mustSucceed(t, "runt run -v NAME:/etc/skel", output, err, ".bashrc")
		copied := filepath.Join("/var/lib/runt/volumes", volume, "_data/.bashrc")
		var stat, original syscall.Stat_t
		if err := syscall.Stat(copied, &stat); err != nil {
			t.Fatalf("the image's .bashrc wasn't copied into the volume: %v", err)
		}
		if err := syscall.Stat(filepath.Join(r.rootfs, "etc/skel/.bashrc"), &original); err != nil {
			t.Fatal(err)
		}
		if stat.Uid != original.Uid || stat.Mode != original.Mode {
			t.Errorf("%s has owner %d and mode %o; want the image's, %d and %o", copied, stat.Uid, stat.Mode, original.Uid, original.Mode)
		}
		observe("%s, owner %d, mode %o, as in the image", copied, stat.Uid, stat.Mode&0o7777)

		output, err = r.runOnce("runt-feature-vol-nocopy", "--rootfs", r.rootfs, "-v", empty+":/etc/skel:nocopy", "--", "/bin/ls", "-A", "/etc/skel")
		mustSucceed(t, "runt run -v NAME:/etc/skel:nocopy", output, err)
		if strings.Contains(output, ".bashrc") {
			t.Errorf("a :nocopy volume was filled from the image:\n%s", output)
		}
	})

	t.Run("WritableUnderReadOnlyRoot", func(t *testing.T) {
		explain(t, "a volume stays writable when the root filesystem is read-only, as a bundle's is")
		r := newRuntTester(t)
		volume := "runt-feature-vol-ro-root-" + nonce
		r.removeVolumeAtEnd(volume)
		bundle, spec := writeFeatureBundle(t, r)
		spec["process"].(map[string]any)["args"] = []string{"/bin/sh", "-c", "touch /mnt/x && echo volume-writable; touch /root-is-read-only"}
		saveBundle(t, bundle, spec)
		output, _ := r.runOnce("runt-feature-vol-ro-root", "--bundle", bundle, "-v", volume+":/mnt")
		for _, want := range []string{"volume-writable", "Read-only file system"} {
			if !strings.Contains(output, want) {
				t.Errorf("output is missing %q:\n%s", want, output)
			}
		}
	})
}

// TestFeatureTmpfs checks tmpfs mounts: --tmpfs /path[:options].
func TestFeatureTmpfs(t *testing.T) {
	explain(t, "tmpfs mounts: an empty filesystem in memory, gone when the container stops (--tmpfs /path)")

	t.Run("InMemoryNotInTheWritableLayer", func(t *testing.T) {
		explain(t, "--tmpfs mounts an empty tmpfs, nosuid,nodev,noexec, whose files never reach the writable layer")
		r := newRuntTester(t)
		name := "runt-feature-tmpfs"
		r.removeAtEnd(name)
		output, err := r.runt("run", "--name", name, "--rootfs", r.rootfs, "--tmpfs", "/scratch", "--", "/bin/sh", "-c",
			"grep ' /scratch ' /proc/mounts; ls -A /scratch; echo in-memory > /scratch/file && cat /scratch/file")
		mustSucceed(t, "runt run --tmpfs", output, err, "in-memory")
		line := mountLine(output, "/scratch")
		if fields := strings.Fields(line + " - - - -"); fields[2] != "tmpfs" || !strings.Contains(fields[3], "nosuid,nodev,noexec") {
			t.Errorf("/scratch isn't a nosuid,nodev,noexec tmpfs:\n%s", output)
		}
		observe("%s", line)
		upper := filepath.Join("/var/lib/runt/containers", containerID(t, name), "upper")
		if _, err := os.Stat(filepath.Join(upper, "scratch/file")); err == nil {
			t.Error("a file written to the tmpfs reached the writable layer")
		}
	})

	t.Run("SizeAndMode", func(t *testing.T) {
		explain(t, "size= limits how much it holds, and mode= sets its permissions")
		r := newRuntTester(t)
		output, _ := r.runOnce("runt-feature-tmpfs-size", "--rootfs", r.rootfs, "--tmpfs", "/scratch:size=1m,mode=700", "--", "/bin/sh", "-c",
			"stat -c mode=%a /scratch; head -c 2097152 /dev/zero > /scratch/big")
		for _, want := range []string{"mode=700", "No space left on device"} {
			if !strings.Contains(output, want) {
				t.Errorf("output is missing %q:\n%s", want, output)
			}
		}
	})

	t.Run("NoexecUnlessExec", func(t *testing.T) {
		explain(t, "programs on it can't be run, unless it is mounted with exec")
		r := newRuntTester(t)
		script := "cp /bin/true /scratch/true && /scratch/true && echo ran"
		output, _ := r.runOnce("runt-feature-tmpfs-noexec", "--rootfs", r.rootfs, "--tmpfs", "/scratch", "--", "/bin/sh", "-c", script)
		if strings.Contains(output, "ran") || !strings.Contains(output, "Permission denied") {
			t.Errorf("a program on a noexec tmpfs ran:\n%s", output)
		}
		output, err := r.runOnce("runt-feature-tmpfs-exec", "--rootfs", r.rootfs, "--tmpfs", "/scratch:exec", "--", "/bin/sh", "-c", script)
		mustSucceed(t, "running a program on an exec tmpfs", output, err, "ran")
	})

	t.Run("InsideAVolume", func(t *testing.T) {
		explain(t, "mounts go in parent-first order, so a tmpfs at /data/cache goes inside a volume at /data")
		r := newRuntTester(t)
		volume := "runt-feature-vol-nested-" + nonce
		r.removeVolumeAtEnd(volume)
		// Given child first, on purpose.
		output, err := r.runOnce("runt-feature-tmpfs-nested", "--rootfs", r.rootfs, "--tmpfs", "/data/cache", "-v", volume+":/data", "--", "/bin/sh", "-c",
			"echo cached > /data/cache/file && cat /proc/mounts")
		mustSucceed(t, "runt run --tmpfs /data/cache -v NAME:/data", output, err)
		if fields := strings.Fields(mountLine(output, "/data/cache") + " - - -"); fields[2] != "tmpfs" {
			t.Errorf("/data/cache isn't a tmpfs:\n%s", output)
		}
		data := filepath.Join("/var/lib/runt/volumes", volume, "_data")
		if _, err := os.Stat(filepath.Join(data, "cache")); err != nil {
			t.Errorf("the tmpfs's mount point wasn't made in the volume: %v", err)
		}
		if _, err := os.Stat(filepath.Join(data, "cache/file")); err == nil {
			t.Error("the file written to the tmpfs reached the volume")
		}
	})
}
