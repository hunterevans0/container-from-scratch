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

func TestFindContainerAndCheckName(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for i, state := range []*State{
		{ID: "abc111111111", Name: "web", Created: now},
		{ID: "abc222222222", Created: now.Add(time.Second)},
		{ID: "def333333333", Created: now.Add(2 * time.Second)},
	} {
		if err := createStateDir(root, state.ID); err != nil {
			t.Fatal(err)
		}
		if err := saveState(root, state); err != nil {
			t.Fatalf("state %d: %v", i, err)
		}
	}

	states, err := listStates(root)
	if err != nil || len(states) != 3 || states[0].ID != "def333333333" {
		t.Fatalf("listStates = %v, %v; want 3, newest first", states, err)
	}
	for ref, want := range map[string]string{"web": "abc111111111", "abc222222222": "abc222222222", "d": "def333333333"} {
		if state, err := findContainer(root, ref); err != nil || state.ID != want {
			t.Errorf("findContainer(%q) = %v, %v; want %s", ref, state, err, want)
		}
	}
	for _, ref := range []string{"abc", "zzz", ""} {
		if _, err := findContainer(root, ref); err == nil {
			t.Errorf("findContainer(%q) succeeded; want an error", ref)
		}
	}

	if err := checkName(root, "db"); err != nil {
		t.Errorf("checkName(db) = %v", err)
	}
	for _, name := range []string{"web", "-x", "a b", "0123456789ab"} {
		if err := checkName(root, name); err == nil {
			t.Errorf("checkName(%q) succeeded; want an error", name)
		}
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
