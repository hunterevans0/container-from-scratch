package runtime

type Config struct {
	// Rootfs is an unpacked root filesystem. The container sees it through
	// an overlay with a writable layer of its own, so the directory itself
	// is never changed, unless DirectRootfs is set.
	Rootfs string
	// DirectRootfs makes the container use Rootfs itself, as runc does with
	// a bundle's root filesystem, instead of an overlay on top of it.
	DirectRootfs bool
	// ReadonlyRootfs mounts the container's root filesystem read-only.
	ReadonlyRootfs bool
	// Image is an OCI image layout directory to run instead of Rootfs,
	// optionally followed by :TAG.
	Image string
	// Bundle is the OCI runtime bundle the configuration was read from, if
	// any (see LoadBundle).
	Bundle  string
	Command []string
	// Env replaces the command's environment. Nil means it inherits runt's.
	Env []string
	// Cwd is the command's working directory inside the container.
	Cwd string
	// User is who the command runs as. Nil means container root.
	User *User
	// Hostname is the container's hostname; empty means "runt".
	Hostname string
	// Name is an optional name to refer to the container by, besides its ID.
	Name string
	// UsernsUser is the user whose /etc/subuid and /etc/subgid ranges the
	// container's IDs map onto. Empty means the invoking user.
	UsernsUser string
	// UIDs and GIDs, when their Size is set, are the ID ranges to map the
	// container onto instead of the ones in /etc/subuid and /etc/subgid.
	UIDs IDRange
	GIDs IDRange
	// AppArmorProfile is the name of a loaded AppArmor profile to confine
	// the command with. Empty means none.
	AppArmorProfile string
	// DebugInit makes the container's init process wait for a debugger to
	// attach before it sets anything up, and prints its host PID.
	DebugInit bool
	// Resources are the container's cgroup limits.
	Resources Resources
	// Detach runs the container in the background, with its output going to
	// its log.
	Detach bool
	// Interactive connects runt's standard input to the command.
	Interactive bool
	// Tty gives the command a pseudo-terminal of its own as its standard
	// input, output, and error.
	Tty bool
}

// User is who a command runs as inside the container.
type User struct {
	UID            uint32   `json:"uid"`
	GID            uint32   `json:"gid"`
	AdditionalGIDs []uint32 `json:"additionalGids,omitempty"`
}

// ExecOptions control how Exec connects the command to runt's terminal.
type ExecOptions struct {
	// Interactive connects runt's standard input to the command.
	Interactive bool
	// Tty gives the command a pseudo-terminal of its own.
	Tty bool
}
