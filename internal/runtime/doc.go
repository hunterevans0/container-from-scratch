// Package runtime is runt's container runtime: everything that makes, runs,
// and manages containers, images, and networks. The runt command, in
// cmd/runt, reads the command line and calls the exported (capitalized)
// functions here, such as Run, Create, Exec, Pull, and CreateNetwork.
//
// # How the files fit together
//
// Making and running a container, in the order it happens:
//
//   - runtime.go: Config, the description of a container to make.
//   - container_linux.go: the supervisor (Run, Create, and supervise), which
//     sets up everything that is done from the host and starts init; and the
//     commands that act on a container afterwards (Start, Stop, Kill,
//     Remove, ListContainers).
//   - runtime_linux.go: Init, the container's first process, which builds
//     the container's view of the system from inside it and runs the
//     command.
//   - state.go: the container's ID and its state.json in /run/runt/<id>.
//   - logs.go: saving the container's output, and runt logs.
//   - attach_linux.go: runt attach, and the supervisor's side of it.
//   - exec_linux.go: runt exec, which runs another command in a running
//     container.
//   - tty_linux.go: pseudo-terminals, for -t.
//   - signals_linux.go: signal names, for runt kill -s.
//
// Isolation and limits:
//
//   - subid.go: which host user IDs container IDs map onto.
//   - capabilities_linux.go, seccomp.go, seccomp_linux*.go,
//     security_linux.go, apparmor_linux.go: the restrictions init applies
//     before running the command.
//   - resources.go, cgroup_linux.go: memory, CPU, process, and disk limits,
//     through a cgroup per container.
//   - lock_linux.go: file locks, wherever runt processes could clash.
//
// Filesystems and images:
//
//   - overlay_linux.go: each container's root filesystem, an overlay of
//     read-only layers under a writable layer of its own.
//   - layer_linux.go: unpacking an image's layers.
//   - image.go: reading OCI image layouts.
//   - imagestore_linux.go: runt's image store (pull, images, rmi).
//   - registry.go: downloading images from registries, and runt login.
//   - reference.go: image names, such as alpine:3.20.
//   - imageconfig.go: an image's default command, environment, and user.
//   - bundle.go: OCI runtime bundles (--bundle) and runt spec.
//
// Networking:
//
//   - network_linux.go: bridge networks, and how a container joins one.
//   - networks_linux.go: runt network create, ls, and rm.
//   - ipam_linux.go: handing out addresses.
//   - netlink_linux.go: asking the kernel to change network interfaces.
//   - iptables_linux.go: running iptables, for NAT and the like.
//   - ports.go, portmap_linux.go: publishing ports (-p).
//   - etc_linux.go: each container's /etc/hosts and /etc/resolv.conf.
//   - dns_linux.go: the DNS server that lets containers find each other by
//     name.
//
// Files named *_test.go are tests, which go test runs (see "Tests" below).
// runtime_unsupported.go stands in for the Linux-only files on other
// systems.
//
// # Reading this code if you don't know Go
//
// A //go:build line at the top of a file is a build constraint: //go:build
// linux means the file is only compiled when building for Linux. A file
// whose name ends in _linux.go, or _linux_amd64.go, is limited the same
// way. runt builds on Windows too, but can only run containers on Linux,
// such as in WSL2.
//
// Packages and names:
//
//   - Every .go file in a directory belongs to one package, here runtime,
//     and they all see each other's names. A function used in one file may
//     well be defined in another.
//   - A name that starts with a capital letter (CreateNetwork) is exported:
//     other packages, such as cmd/runt, can use it. Lower case
//     (joinNetwork) means private to this package.
//   - import lists the other packages a file uses, from Go's standard
//     library (os, fmt, net, syscall, and so on). A package is then used by
//     its name, as in os.ReadFile. An import can rename one, as
//     goruntime "runtime" does for Go's own runtime package, since this
//     package is called runtime too.
//
// Variables and types:
//
//   - x := value declares a variable and sets it, with its type taken from
//     the value. var x T declares one set to its type's zero value: 0, "",
//     false, or nil (no value) for pointers, slices, maps, channels,
//     functions, and interfaces. Several can be set at once: a, b = 1, 2.
//   - const declares constants, which can only be numbers, strings, and
//     booleans. 0o755 is a number in octal, as file permissions are written,
//     0x7fff in hex, and 1 << 20 is 1 shifted left 20 bits, 2 to the 20th.
//   - type T struct { ... } defines a record type. Its fields can have tags,
//     such as `json:"name"`, telling the encoding/json package what to call
//     the field in JSON.
//   - *T is a pointer to a T, &x is the address of x, and *p is the value p
//     points to. &T{field: value} makes a T and returns a pointer to it.
//     Fields left out of such a literal get their zero values.
//   - []T is a slice, a growable list. append(list, x) returns it with x
//     added, and list[i:j] is the part from i up to (not including) j.
//     [4]byte is an array, whose size is fixed.
//   - map[K]V is a hash map. m[k] looks up a key, giving the zero value if
//     it isn't there; v, ok := m[k] also says whether it was.
//
// Functions and methods:
//
//   - Functions can return several values. By convention the last one is an
//     error, which is nil if all went well. So most calls are followed by
//     "if err != nil { return ..., err }", which passes the failure up to the
//     caller. Go has no exceptions; errors are ordinary values.
//   - fmt.Errorf("doing X: %w", err) wraps an error with what was being
//     done. errors.Is(err, target) looks through the wrapping for a
//     particular error, such as os.ErrNotExist ("no such file") or
//     syscall.EEXIST ("already exists"), and errors.As for one of a given
//     type.
//   - defer f() runs f when the function it is in returns, however it
//     returns, error or not. It is how files get closed and locks released.
//     Deferred calls run last-in, first-out.
//   - func (t *T) name() { ... } defines a method on T, called as t.name().
//     t is its receiver, as this or self are in other languages.
//   - func(...) { ... } written inline is an anonymous function. It can use
//     the variables around it, even after the function that made it has
//     returned (a closure), and is often stored in a variable, passed to
//     another function, or started as a goroutine.
//
// Interfaces:
//
//   - An interface type lists methods, and any type that has those methods
//     satisfies it, with no declaration needed. error is one: anything with
//     an Error() string method is an error. io.Reader (anything with a Read
//     method) and io.Writer (Write) are others, which let the same code read
//     from a file, a pipe, a socket, or a string.
//   - x.(T) is a type assertion: it takes the T out of the interface value
//     x. v, ok := x.(T) also reports whether x holds a T, rather than
//     stopping the program if it doesn't. any is the interface every type
//     satisfies.
//
// Control flow:
//
//   - for is Go's only loop: for i := 0; i < n; i++ { ... }, for cond
//     { ... }, for { ... } (forever, until break or return), or
//     for i, x := range list { ... } over a slice, map, string, channel, or
//     number. _ stands in for a value that isn't needed.
//   - if and switch can start with a short statement: if err := f(); err !=
//     nil { ... } declares err for just that if. A switch's cases don't fall
//     through into each other, and a switch with no value tests each case's
//     condition in turn.
//
// Concurrency:
//
//   - go f() starts f running concurrently, in a goroutine, a lightweight
//     thread. The program doesn't wait for it.
//   - A channel (chan T) passes values between goroutines: ch <- v sends v,
//     and <-ch waits for a value and receives it. close(ch) says no more
//     will come. A chan struct{} carries no data; closing it is the signal.
//   - select waits on several channel operations, and does whichever can go
//     ahead first. With a default case, it doesn't wait at all.
//   - sync.Mutex makes goroutines take turns with shared data: Lock waits
//     until no other goroutine holds it, and Unlock lets the next one in.
//     sync.WaitGroup counts goroutines still running, and Wait waits for
//     them all to finish.
//
// Talking to the kernel:
//
//   - The syscall package makes Linux system calls directly: syscall.Mount,
//     syscall.Kill, and so on, or syscall.Syscall(number, args...) for those
//     it has no function for. These return an error number (errno), which
//     is 0 on success.
//   - unsafe.Pointer(&x) is x's bare memory address, which some system calls
//     take, to read or fill in a struct laid out as the kernel's C one is.
//
// # Tests
//
// A file named *_test.go holds tests, compiled only by go test. Each test
// is a function named TestSomething taking a *testing.T, which it reports
// problems through: t.Errorf records a failure and carries on, t.Fatalf
// records one and stops the test, t.Run runs a named subtest, t.Helper
// marks a function as a helper so failures are reported at its caller's
// line, t.TempDir makes a directory that is removed afterwards, and
// t.Cleanup registers a function to run when the test ends. Many tests here
// are table-driven: a slice of cases, each an input and the output wanted,
// checked in a loop.
package runtime
