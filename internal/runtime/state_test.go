package runtime

import (
	"regexp"
	"slices"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(id) {
		t.Errorf("newID() = %q; want 12 hex characters", id)
	}
	if other, _ := newID(); other == id {
		t.Errorf("newID() returned %q twice", id)
	}
}

func TestStateRoundTrip(t *testing.T) {
	root := t.TempDir()
	exitCode := 3
	state := &State{
		ID:        "0123456789ab",
		Status:    StatusStopped,
		Rootfs:    "/var/lib/runt/rootfs",
		Command:   []string{"/bin/sh", "-c", "exit 3"},
		ExitCode:  &exitCode,
		Cgroup:    "/sys/fs/cgroup/runt/0123456789ab",
		Resources: Resources{Memory: 64 << 20, PidsLimit: 10},
		Created:   time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	if err := createStateDir(root, state.ID); err != nil {
		t.Fatal(err)
	}
	if err := createStateDir(root, state.ID); err == nil {
		t.Error("creating the same state directory twice succeeded")
	}
	if err := saveState(root, state); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(root, state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != state.ID || got.Status != state.Status || !slices.Equal(got.Command, state.Command) ||
		got.ExitCode == nil || *got.ExitCode != exitCode || got.Resources.Memory != state.Resources.Memory ||
		!got.Created.Equal(state.Created) || !got.Started.IsZero() {
		t.Errorf("loadState() = %+v; want %+v", got, state)
	}
}
