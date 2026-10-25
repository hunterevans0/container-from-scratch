//go:build integration && linux

// Package integration runs the real runt binary against a real rootfs. It
// needs root and the rootfs from make rootfs, so it only builds with the
// integration tag: run it with make integration.
//
// Not tested yet: containers started from the same rootfs share its user-ID
// range, so a process in one is the same host user as a process in the
// other. Their own files are kept apart by a writable layer each
// (filesystem_test.go), but a shared bind mount would be open to both.
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// state is the part of /run/runt/<id>/state.json these tests read.
type state struct {
	Status    string `json:"status"`
	Pid       int    `json:"pid"`
	Cgroup    string `json:"cgroup"`
	IPAddress string `json:"ipAddress"`
	Ports     []struct {
		HostIP        string `json:"hostIp"`
		HostPort      int    `json:"hostPort"`
		ContainerPort int    `json:"containerPort"`
		Protocol      string `json:"protocol"`
	} `json:"ports"`
}

// envOr returns the environment variable key, or fallback if it is unset.
func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// runtTester runs runt commands for one test.
type runtTester struct {
	t      *testing.T
	binary string
	rootfs string
}

// newRuntTester skips the test unless it can start containers: it needs root,
// a built runt binary (RUNT_BINARY), and a rootfs (RUNT_ROOTFS).
func newRuntTester(t *testing.T) *runtTester {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("starting containers needs root; run make integration")
	}
	binary, err := filepath.Abs(envOr("RUNT_BINARY", "../runt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("no runt binary (%v); run make build or set RUNT_BINARY", err)
	}
	rootfs := envOr("RUNT_ROOTFS", "/var/lib/runt/rootfs")
	if _, err := os.Stat(rootfs); err != nil {
		t.Skipf("no rootfs (%v); run make rootfs or set RUNT_ROOTFS", err)
	}
	return &runtTester{t: t, binary: binary, rootfs: rootfs}
}

// runt runs runt with args and returns its combined output.
func (r *runtTester) runt(args ...string) (string, error) {
	output, err := exec.Command(r.binary, args...).CombinedOutput()
	return string(output), err
}

// start runs command in a new background container called name and returns
// its ID once it is running. The container is removed when the test ends.
func (r *runtTester) start(name string, command ...string) string {
	r.t.Helper()
	return r.startWith(name, []string{"--rootfs", r.rootfs}, command...)
}

// startWith is start with other options for runt run -d, such as -t.
func (r *runtTester) startWith(name string, options []string, command ...string) string {
	r.t.Helper()
	r.removeAtEnd(name)
	args := append([]string{"run", "-d", "--name", name}, options...)
	args = append(append(args, "--"), command...)
	output, err := r.runt(args...)
	if err != nil {
		r.t.Fatalf("runt %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	id := strings.TrimSpace(output)
	deadline := time.Now().Add(5 * time.Second)
	for readState(r.t, id).Status != "running" {
		if time.Now().After(deadline) {
			r.t.Fatalf("container %s did not start running", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return id
}

// removeAtEnd removes container name, if there is one, now and when the
// test ends: one left behind by an earlier run that crashed, and the one the
// test is about to make.
func (r *runtTester) removeAtEnd(name string) {
	r.t.Helper()
	r.runt("rm", "-f", name)
	r.t.Cleanup(func() {
		if output, err := r.runt("rm", "-f", name); err != nil && !strings.Contains(output, "no such container") {
			r.t.Errorf("runt rm -f %s: %v\n%s", name, err, output)
		}
	})
}

// exec runs a shell script inside container and returns its output.
func (r *runtTester) exec(container, script string) (string, error) {
	return r.runt("exec", container, "/bin/sh", "-c", script)
}

// readState reads what runt recorded about container id.
func readState(t *testing.T, id string) state {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/run/runt", id, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("state of %s: %v", id, err)
	}
	return s
}

// TestContainersAreIsolated starts two containers and has the first one try
// to see, kill, and take over the second.
func TestContainersAreIsolated(t *testing.T) {
	r := newRuntTester(t)
	// The sleep times make each container's process easy to recognize.
	r.start("runt-iso-a", "/bin/sleep", "3001")
	idB := r.start("runt-iso-b", "/bin/sleep", "3002")
	b := readState(t, idB)

	t.Run("cannot see processes", func(t *testing.T) {
		script := `cat /proc/[0-9]*/cmdline | tr '\0' ' '`
		output, err := r.exec("runt-iso-a", script)
		if err != nil {
			t.Fatalf("A ran %q: %v\n%s", script, err, output)
		}
		// Seeing its own sleep shows the listing really worked.
		if !strings.Contains(output, "3001") {
			t.Errorf("A ran %q: got %q, want its own sleep 3001 listed", script, output)
		}
		if strings.Contains(output, "3002") {
			t.Errorf("A ran %q: got %q, want B's sleep 3002 hidden", script, output)
		}
	})

	t.Run("cannot kill by host PID", func(t *testing.T) {
		script := fmt.Sprintf("kill -9 %d", b.Pid)
		if output, err := r.exec("runt-iso-a", script); err == nil {
			t.Errorf("A ran %q: succeeded with %q, want it to fail", script, output)
		}
		if err := syscall.Kill(b.Pid, 0); err != nil {
			t.Errorf("B's init (host PID %d) is gone after A ran %q: %v", b.Pid, script, err)
		}
		if status := readState(t, idB).Status; status != "running" {
			t.Errorf("B's status is %q after A ran %q, want running", status, script)
		}
	})

	t.Run("cannot join cgroup", func(t *testing.T) {
		procs := filepath.Join(b.Cgroup, "cgroup.procs")
		before, err := os.ReadFile(procs)
		if err != nil {
			t.Fatal(err)
		}
		// Moving A's init into B's cgroup would put A under B's limits and
		// let it see B's usage.
		script := "echo 1 > " + procs
		if output, err := r.exec("runt-iso-a", script); err == nil {
			t.Errorf("A ran %q: succeeded with %q, want it to fail", script, output)
		}
		after, err := os.ReadFile(procs)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Errorf("B's cgroup.procs changed after A ran %q: was %q, now %q", script, before, after)
		}
	})
}
