package runtime

type Config struct {
	Rootfs  string
	Command []string
	// Name is an optional name to refer to the container by, besides its ID.
	Name string
	// UsernsUser is the user whose /etc/subuid and /etc/subgid ranges the
	// container's IDs map onto. Empty means the invoking user.
	UsernsUser string
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
}
