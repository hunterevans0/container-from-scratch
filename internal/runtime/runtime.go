package runtime

// The types that describe a container and the options of the commands that
// work with one, shared by every platform (this file has no build
// constraint). cmd/runt fills in a Config from the command line and hands it
// to Run or Create (in container_linux.go). For an overview of the package,
// and how to read Go, see doc.go.

import "time"

// Config is everything runt needs to know to make and run a container, as
// runt run and runt create take it from their options. The comment on each
// field says what it means. Fields left unset keep their zero value: "" for
// strings, false, 0, or nil, which each field's comment explains the meaning
// of where it matters.
//
// A *string, as Entrypoint is, is a pointer to a string: nil means "not
// given", which is different from "given, and empty". Likewise *User.
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
	// Image is the image to run instead of Rootfs: a name or ID in runt's
	// image store, or an OCI image layout directory, optionally followed by
	// :TAG. Its configuration fills in what the options below leave out
	// (see applyImageConfig).
	Image string
	// Entrypoint, if set, replaces the image's entrypoint; empty means
	// none.
	Entrypoint *string
	// Bundle is the OCI runtime bundle the configuration was read from, if
	// any (see LoadBundle).
	Bundle  string
	Command []string
	// Env replaces the command's environment. Nil means it inherits runt's.
	Env []string
	// SetEnv are NAME=VALUE settings (-e) to make on top of the
	// environment.
	SetEnv []string
	// Cwd is the command's working directory inside the container. It is
	// created if it doesn't exist.
	Cwd string
	// User is who the command runs as. Nil means container root.
	User *User
	// UserSpec, if set, is who the command runs as instead of User:
	// USER[:GROUP], by name or ID, looked up in the container's /etc/passwd
	// and /etc/group (see resolveUser).
	UserSpec string
	// Hostname is the container's hostname; empty means "runt".
	Hostname string
	// Network is NetworkBridge (the default, if empty), NetworkNone, or the
	// name of a network made with CreateNetwork.
	Network string
	// Ports are the container's ports to publish on the host.
	Ports []PortMapping
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
	// Interactive connects runt's standard input to the command. For a
	// container in the background, its input stays open for runt attach.
	Interactive bool
	// Tty gives the command a pseudo-terminal of its own as its standard
	// input, output, and error.
	Tty bool
}

// Networks a container can be on.
const (
	// NetworkBridge plugs the container into a bridge on the host, with an
	// address of its own and NAT to the outside world.
	NetworkBridge = "bridge"
	// NetworkNone gives the container only a loopback interface.
	NetworkNone = "none"
)

// Network is a network containers can be on: the default bridge network, or
// one made with CreateNetwork, whose containers find each other by name.
// The text in backquotes after each field is a tag: it gives the field's
// name in JSON, as networks are saved on disk, and omitempty (or omitzero,
// for a time) leaves the field out when it is empty.
type Network struct {
	Name string `json:"name"`
	// ID is set on networks made with CreateNetwork.
	ID string `json:"id,omitempty"`
	// Bridge is the network's bridge on the host, and Subnet its range of
	// addresses. NetworkNone has neither.
	Bridge  string    `json:"bridge,omitempty"`
	Subnet  string    `json:"subnet,omitempty"`
	Created time.Time `json:"created,omitzero"`
}

// User is who a command runs as inside the container: a user ID, a group ID,
// and any other groups the user is in. uint32 is an unsigned (never
// negative) 32-bit number, the kernel's type for IDs.
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

// AttachOptions control how Attach connects to a container.
type AttachOptions struct {
	// Stdin sends runt's input to the command, if the container's input is
	// open (it was made with -i).
	Stdin bool
	// SigProxy passes signals runt gets on to the container, when it
	// doesn't have a terminal.
	SigProxy bool
	// Start starts a created container once attached, as runt start -a
	// does, so none of its output is missed.
	Start bool
}
