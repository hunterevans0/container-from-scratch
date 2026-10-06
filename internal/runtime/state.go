package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Status  string   `json:"status"`
	Rootfs  string   `json:"rootfs"`
	Command []string `json:"command"`
	// Pid is the host PID of the container's init process, while it runs.
	Pid int `json:"pid,omitempty"`
	// ExitCode is set once the container has stopped.
	ExitCode *int `json:"exitCode,omitempty"`
	// Cgroup is the container's cgroup directory, if it has one.
	Cgroup    string    `json:"cgroup,omitempty"`
	Resources Resources `json:"resources"`
	Created   time.Time `json:"created"`
	Started   time.Time `json:"started,omitzero"`
	Finished  time.Time `json:"finished,omitzero"`
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
