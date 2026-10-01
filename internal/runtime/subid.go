package runtime

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// IDRange is a block of host IDs that container IDs 0..Size-1 map onto.
type IDRange struct {
	Base int
	Size int
}

// defaultIDRange is used when no user was asked for and the invoking user has
// no entry in /etc/subuid or /etc/subgid. It matches the first range Ubuntu
// hands out.
var defaultIDRange = IDRange{Base: 100000, Size: 65536}

// usernsUser picks whose subordinate ID range to use: the user asked for, else
// whoever ran sudo, else the current user.
func usernsUser(requested string) string {
	if requested != "" {
		return requested
	}
	if name := os.Getenv("SUDO_USER"); name != "" {
		return name
	}
	return os.Getenv("USER")
}

// subIDRange reads user's range from path, which is /etc/subuid or
// /etc/subgid. If the user has no entry it falls back to defaultIDRange,
// unless the user was explicitly asked for.
func subIDRange(path, user string, explicit bool) (IDRange, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return defaultIDRange, nil
		}
		return IDRange{}, err
	}
	defer file.Close()

	idRange, found, err := parseSubID(file, user)
	if err != nil {
		return IDRange{}, fmt.Errorf("%s: %w", path, err)
	}
	if !found {
		if explicit {
			return IDRange{}, fmt.Errorf("no entry for %q in %s", user, path)
		}
		return defaultIDRange, nil
	}
	return idRange, nil
}

// parseSubID returns the first range belonging to user. Each line is
// "name:start:count" (see subuid(5)), where name is a login name or a
// numeric ID.
func parseSubID(r io.Reader, user string) (IDRange, bool, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 3 || fields[0] != user {
			continue
		}
		base, err := strconv.Atoi(fields[1])
		if err != nil || base < 0 {
			return IDRange{}, false, fmt.Errorf("invalid start in %q", line)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size <= 0 {
			return IDRange{}, false, fmt.Errorf("invalid count in %q", line)
		}
		return IDRange{Base: base, Size: size}, true, nil
	}
	return IDRange{}, false, scanner.Err()
}
