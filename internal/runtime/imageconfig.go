package runtime

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
)

// An image's configuration says how to run it, with the same defaults a
// Dockerfile's ENTRYPOINT, CMD, ENV, WORKDIR, and USER set. Options on the
// command line override them, as with docker run.

// defaultPath is the PATH a container gets if its image doesn't set one, as
// in Docker.
const defaultPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// applyImageConfig fills in what config leaves out from an image's
// configuration:
//
//   - The command is the entrypoint followed by the arguments given, or by
//     the image's default command (CMD) if none were. --entrypoint replaces
//     the image's entrypoint and drops its default command, as in Docker.
//   - The environment is the image's, with PATH set if the image doesn't,
//     TERM=xterm with -t, and the -e settings on top.
//   - The working directory is the image's unless -w gave one, and "/" if
//     neither did.
//   - The user is the image's unless -u gave one.
func applyImageConfig(config *Config, image *ociImage) error {
	defaults := image.config.Config
	entrypoint, cmd := defaults.Entrypoint, defaults.Cmd
	if config.Entrypoint != nil {
		entrypoint, cmd = nil, nil
		if *config.Entrypoint != "" {
			entrypoint = []string{*config.Entrypoint}
		}
	}
	if len(config.Command) > 0 {
		cmd = config.Command
	}
	config.Command = append(append([]string{}, entrypoint...), cmd...)
	if len(config.Command) == 0 {
		return errors.New("the image has no default command; give one after the options")
	}

	env := append([]string{}, defaults.Env...)
	if !hasEnv(env, "PATH") {
		env = append([]string{defaultPath}, env...)
	}
	if config.Tty && !hasEnv(env, "TERM") {
		env = append(env, "TERM=xterm")
	}
	config.Env = mergeEnv(env, config.SetEnv)
	config.SetEnv = nil

	if config.Cwd == "" {
		config.Cwd = defaults.WorkingDir
	}
	if config.Cwd == "" {
		config.Cwd = "/"
	}
	if config.UserSpec == "" && config.User == nil {
		config.UserSpec = defaults.User
	}
	return checkCwd(config.Cwd)
}

func checkCwd(cwd string) error {
	if cwd != "" && !path.IsAbs(cwd) {
		return fmt.Errorf("the working directory %q must be an absolute path", cwd)
	}
	return nil
}

// hasEnv reports whether env sets the variable name.
func hasEnv(env []string, name string) bool {
	for _, variable := range env {
		if key, _, _ := strings.Cut(variable, "="); key == name {
			return true
		}
	}
	return false
}

// mergeEnv returns env with each NAME=VALUE in overrides set, replacing any
// earlier setting of NAME.
func mergeEnv(env, overrides []string) []string {
	merged := append([]string{}, env...)
	for _, override := range overrides {
		name, _, _ := strings.Cut(override, "=")
		replaced := false
		for i, variable := range merged {
			if key, _, _ := strings.Cut(variable, "="); key == name {
				merged[i] = override
				replaced = true
			}
		}
		if !replaced {
			merged = append(merged, override)
		}
	}
	return merged
}

// Users and groups are looked up in the container's own /etc/passwd and
// /etc/group, from inside it, as runc does: a name means whatever the image
// says it means. The files come from the image, so they are read with care:
// only regular files, and only so much of them.

// maxAccountFile caps how much of /etc/passwd or /etc/group is read.
const maxAccountFile = 16 << 20

// passwdEntry is a line of /etc/passwd.
type passwdEntry struct {
	name string
	uid  int
	gid  int
	home string
}

// groupEntry is a line of /etc/group.
type groupEntry struct {
	name    string
	gid     int
	members []string
}

// openAccountFile opens /etc/passwd or /etc/group, or returns nil if there
// isn't one.
func openAccountFile(name string) (io.ReadCloser, error) {
	file, err := os.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	return file, nil
}

// readAccountFile calls entry with the colon-separated fields of each line of
// /etc/passwd or /etc/group.
func readAccountFile(name string, entry func(fields []string)) error {
	file, err := openAccountFile(name)
	if err != nil || file == nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, maxAccountFile))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entry(strings.Split(line, ":"))
	}
	return scanner.Err()
}

func readPasswd(name string) ([]passwdEntry, error) {
	var entries []passwdEntry
	err := readAccountFile(name, func(fields []string) {
		// name:password:uid:gid:gecos:home:shell
		if len(fields) < 7 {
			return
		}
		uid, err1 := strconv.Atoi(fields[2])
		gid, err2 := strconv.Atoi(fields[3])
		if err1 == nil && err2 == nil {
			entries = append(entries, passwdEntry{name: fields[0], uid: uid, gid: gid, home: fields[5]})
		}
	})
	return entries, err
}

func readGroup(name string) ([]groupEntry, error) {
	var entries []groupEntry
	err := readAccountFile(name, func(fields []string) {
		// name:password:gid:member,member,...
		if len(fields) < 4 {
			return
		}
		gid, err := strconv.Atoi(fields[2])
		if err != nil {
			return
		}
		var members []string
		if fields[3] != "" {
			members = strings.Split(fields[3], ",")
		}
		entries = append(entries, groupEntry{name: fields[0], gid: gid, members: members})
	})
	return entries, err
}

// resolveUser works out who to run as from a user specification, as
// docker run -u and an image's USER take it: USER or USER:GROUP, each a
// name or a numeric ID. A numeric ID needn't be in /etc/passwd or
// /etc/group, but a name must be. Without a group, the user's primary
// group from /etc/passwd is used, or 0. The user also gets the groups that
// list them as a member in /etc/group. home is the user's home directory,
// or "/" if they have none.
func resolveUser(spec string, passwd []passwdEntry, groups []groupEntry) (user User, home string, err error) {
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	if userPart == "" {
		userPart = "0"
	}
	home = "/"
	name := ""
	uid, numeric := parseID(userPart)
	var gid int
	found := false
	for _, entry := range passwd {
		if (numeric && entry.uid == uid) || (!numeric && entry.name == userPart) {
			uid, gid, home, name, found = entry.uid, entry.gid, entry.home, entry.name, true
			break
		}
	}
	if !found && !numeric {
		return User{}, "", fmt.Errorf("no user %q in the container's /etc/passwd", userPart)
	}
	if home == "" {
		home = "/"
	}

	if hasGroup && groupPart != "" {
		var numericGroup bool
		gid, numericGroup = parseID(groupPart)
		if !numericGroup {
			found := false
			for _, entry := range groups {
				if entry.name == groupPart {
					gid, found = entry.gid, true
					break
				}
			}
			if !found {
				return User{}, "", fmt.Errorf("no group %q in the container's /etc/group", groupPart)
			}
		}
	}

	user = User{UID: uint32(uid), GID: uint32(gid)}
	if name != "" {
		for _, entry := range groups {
			if entry.gid != gid && slices.Contains(entry.members, name) && !slices.Contains(user.AdditionalGIDs, uint32(entry.gid)) {
				user.AdditionalGIDs = append(user.AdditionalGIDs, uint32(entry.gid))
			}
		}
	}
	return user, home, nil
}

// parseID parses a numeric user or group ID.
func parseID(s string) (int, bool) {
	id, err := strconv.ParseUint(s, 10, 31)
	return int(id), err == nil
}

// homeOf returns the home directory of uid in /etc/passwd, or "/".
func homeOf(uid uint32, passwd []passwdEntry) string {
	for _, entry := range passwd {
		if entry.uid == int(uid) && entry.home != "" {
			return entry.home
		}
	}
	return "/"
}
