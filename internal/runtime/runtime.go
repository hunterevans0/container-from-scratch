package runtime

type Config struct {
	Rootfs  string
	Command []string
	// UsernsUser is the user whose /etc/subuid and /etc/subgid ranges the
	// container's IDs map onto. Empty means the invoking user.
	UsernsUser string
}
