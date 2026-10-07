package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// StateRoot holds a directory per container, named by its ID. /run is a
// tmpfs, so the state is gone after a reboot, like the containers.
const StateRoot = "/run/runt"

// Container statuses.
const (
	StatusCreated = "created"
	StatusRunning = "running"
	StatusStopped = "stopped"
)

// State is what runt records about a container, in StateRoot/<id>/state.json.
type State struct {
	ID      string   `json:"id"`
	Name    string   `json:"name,omitempty"`
	Status  string   `json:"status"`
	Rootfs  string   `json:"rootfs"`
	Command []string `json:"command"`
	// Pid is the host PID of the container's init process, while it is
	// created or running.
	Pid int `json:"pid,omitempty"`
	// ExitCode is set once the container has stopped.
	ExitCode *int `json:"exitCode,omitempty"`
	// Cgroup is the container's cgroup directory, if it has one.
	Cgroup string `json:"cgroup,omitempty"`
	// AppArmorProfile confines the command and anything run with exec.
	AppArmorProfile string    `json:"apparmorProfile,omitempty"`
	Resources       Resources `json:"resources"`
	Created         time.Time `json:"created"`
	Started         time.Time `json:"started,omitzero"`
	Finished        time.Time `json:"finished,omitzero"`
}

// newID returns a random 12-character hex ID, the length Docker shows.
func newID() (string, error) {
	var id [6]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate container ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

// stateDir returns the directory holding the state of container id.
func stateDir(root, id string) string {
	return filepath.Join(root, id)
}

// createStateDir makes the state directory for a new container. It fails if
// the directory already exists, so two containers can't share an ID.
func createStateDir(root, id string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create state root: %w", err)
	}
	if err := os.Mkdir(stateDir(root, id), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	return nil
}

// saveState writes state.json. It writes a temporary file and renames it, so
// a reader never sees a partly written file.
func saveState(root string, state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := stateDir(root, state.ID)
	temp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write container state: %w", err)
	}
	if err := os.Rename(temp, filepath.Join(dir, "state.json")); err != nil {
		return fmt.Errorf("write container state: %w", err)
	}
	return nil
}

// loadState reads the state of container id.
func loadState(root, id string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(stateDir(root, id), "state.json"))
	if err != nil {
		return nil, fmt.Errorf("read container state: %w", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse container state %q: %w", id, err)
	}
	return &state, nil
}

// listStates returns the state of every container, newest first.
// Directories without a readable state.json, such as one being created or
// removed at that moment, are skipped.
func listStates(root string) ([]*State, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var states []*State
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		state, err := loadState(root, entry.Name())
		if err != nil {
			continue
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Created.After(states[j].Created) })
	return states, nil
}

// findContainer looks a container up by name, full ID, or the start of an
// ID, as Docker does. An ID prefix must match only one container.
func findContainer(root, ref string) (*State, error) {
	if ref == "" {
		return nil, errors.New("container name or ID is empty")
	}
	states, err := listStates(root)
	if err != nil {
		return nil, err
	}
	var matches []*State
	for _, state := range states {
		if state.ID == ref || state.Name == ref {
			return state, nil
		}
		if strings.HasPrefix(state.ID, ref) {
			matches = append(matches, state)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no such container: %s", ref)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("%q matches more than one container; use more of the ID", ref)
	}
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// checkName rejects a name that is malformed, looks like an ID, or is
// already taken.
func checkName(root, name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid container name %q: use letters, digits, and _.- and start with a letter or digit", name)
	}
	if regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(name) {
		return fmt.Errorf("invalid container name %q: it looks like a container ID", name)
	}
	states, err := listStates(root)
	if err != nil {
		return err
	}
	for _, state := range states {
		if state.Name == name {
			return fmt.Errorf("the name %q is already used by container %s; remove it with runt rm", name, state.ID)
		}
	}
	return nil
}
