//go:build linux

package runtime

// Named volumes: runt volume create, ls, and rm, and the volumes containers
// make for themselves with -v NAME:/path. Mounting them is in
// mounts_linux.go. For how to read the Go here, see doc.go.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A volume is a directory runt keeps for containers to mount, which outlives
// them, as a database's files should outlive the container running it. Each
// one is a directory in volumeRoot, named after the volume, as Docker keeps
// them in /var/lib/docker/volumes:
//
//   - _data: the volume's files, which containers see.
//   - volume.json: its name and when it was made.
//
// A lock in volumeRoot keeps a volume from being removed while a container
// is being made with it. Containers hold it from making their volumes until
// their state, which lists them, is saved (see supervise).
const volumeRoot = DataRoot + "/volumes"

// lockVolumes locks the volumes, shared or exclusive, as lockNetworks does
// the networks. It returns a function that releases the lock.
func lockVolumes(exclusive bool) (func(), error) {
	if err := os.MkdirAll(volumeRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", volumeRoot, err)
	}
	unlock, err := lockFile(filepath.Join(volumeRoot, ".lock"), exclusive)
	if err != nil {
		return nil, fmt.Errorf("lock volumes: %w", err)
	}
	return unlock, nil
}

// volumeData is the directory holding volume name's files.
func volumeData(name string) string {
	return filepath.Join(volumeRoot, name, "_data")
}

// readVolume reads what runt keeps about the volume called name.
func readVolume(name string) (*Volume, error) {
	// validName keeps a name such as ../x from leading out of volumeRoot.
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("no such volume: %s", name)
	}
	var volume Volume
	err := readJSONFile(filepath.Join(volumeRoot, name, "volume.json"), &volume)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no such volume: %s", name)
	}
	if err != nil {
		return nil, fmt.Errorf("read volume %s: %w", name, err)
	}
	return &volume, nil
}

// ensureVolume returns the volume called name, making it if there is none,
// with its files' directory owned by container root of the given ID ranges,
// so the container can write to it. created says whether it was made just
// now. The caller holds the volumes' lock.
//
// A new volume is put together in a directory of its own and then renamed
// into place, so a volume is either all there or not there at all, even if
// runt is killed halfway.
func ensureVolume(name string, uids, gids IDRange) (volume *Volume, created bool, err error) {
	if !validName.MatchString(name) {
		return nil, false, fmt.Errorf("invalid volume name %q: use letters, digits, and _.- and start with a letter or digit", name)
	}
	if volume, err := readVolume(name); err == nil {
		return volume, false, nil
	}
	// The name starts with a dot, so it can't be a volume's.
	temp, err := os.MkdirTemp(volumeRoot, ".new-")
	if err != nil {
		return nil, false, fmt.Errorf("create volume %s: %w", name, err)
	}
	defer os.RemoveAll(temp)
	volume = &Volume{Name: name, Mountpoint: volumeData(name), Created: time.Now()}
	data, err := json.MarshalIndent(volume, "", "  ")
	if err != nil {
		return nil, false, err
	}
	// The volume's own directory is host root's alone: the container sees
	// _data through a bind mount runt makes as host root, which doesn't
	// need a way in through the directories above it.
	files := filepath.Join(temp, "_data")
	for _, step := range []func() error{
		func() error { return os.Chmod(temp, 0o700) },
		func() error { return os.Mkdir(files, 0o755) },
		func() error { return os.Chmod(files, 0o755) }, // undo the umask
		func() error { return os.Chown(files, uids.Base, gids.Base) },
		func() error { return os.WriteFile(filepath.Join(temp, "volume.json"), append(data, '\n'), 0o600) },
		func() error { return os.Rename(temp, filepath.Join(volumeRoot, name)) },
	} {
		if err := step(); err != nil {
			return nil, false, fmt.Errorf("create volume %s: %w", name, err)
		}
	}
	return volume, true, nil
}

// CreateVolume makes a volume called name, owned by container root of the
// invoking user's ID ranges (see idRanges), and returns it. As with docker
// volume create, a volume that already exists is returned as it is.
func CreateVolume(name string) (*Volume, error) {
	uids, gids, err := idRanges(Config{})
	if err != nil {
		return nil, err
	}
	unlock, err := lockVolumes(true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	volume, _, err := ensureVolume(name, uids, gids)
	return volume, err
}

// ListVolumes returns every volume, by name.
func ListVolumes() ([]Volume, error) {
	unlock, err := lockVolumes(false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, err := os.ReadDir(volumeRoot)
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	var volumes []Volume
	for _, entry := range entries {
		// Skip the lock, and volumes being made.
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		volume, err := readVolume(entry.Name())
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, *volume)
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	return volumes, nil
}

// RemoveVolume deletes a volume and its files. As with Docker, a volume a
// container uses can't be removed, even if the container has stopped,
// until the container is removed (runt rm).
func RemoveVolume(name string) error {
	unlock, err := lockVolumes(true)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := readVolume(name); err != nil {
		return err
	}
	states, err := listStates(StateRoot)
	if err != nil {
		return err
	}
	for _, state := range states {
		for _, mount := range state.Mounts {
			if mount.Type == MountVolume && mount.Source == name {
				return fmt.Errorf("volume %s is in use by container %s; remove the container first", name, state.ID)
			}
		}
	}
	if err := os.RemoveAll(filepath.Join(volumeRoot, name)); err != nil {
		return fmt.Errorf("remove volume %s: %w", name, err)
	}
	return nil
}
