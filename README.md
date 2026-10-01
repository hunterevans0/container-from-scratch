# runt

`runt` is a small container runtime built from scratch in Go. It demonstrates the core Linux process-isolation path:

- `CLONE_NEWUSER` for a user namespace: root inside the container is an unprivileged host uid, taken from `/etc/subuid` and `/etc/subgid` (usually 100000)
- `CLONE_NEWUTS` for an isolated hostname
- `CLONE_NEWPID` for a separate process-ID namespace
- `CLONE_NEWNS` for a separate mount namespace, made private so mounts don't leak to the host
- `CLONE_NEWNET` for a separate network with only a loopback interface (no internet access yet)
- `CLONE_NEWIPC` for separate shared memory and message queues
- `CLONE_NEWCGROUP` so the container sees its own cgroup as the root (`0::/`)
- `pivot_root` for a filesystem boundary, with the host's root detached
- a fresh `/proc`, so `ps` and `top` only see the container's processes
- a read-only `/sys` that shows only the container's own network interfaces
- a minimal `/dev` on tmpfs with `null`, `zero`, `full`, `random`, `urandom`, `tty`, a private `/dev/pts`, `/dev/shm`, and `/dev/mqueue`
- Linux capabilities dropped to Docker's default set, so container root can't mount filesystems, change the hostname, or reconfigure the network

This is educational code, not a production container runtime. It currently requires Linux and root privileges for `run`.

## Setup on Windows

Go 1.27 is installed on the Windows host. Container execution needs Linux, so development runs inside Ubuntu WSL2. From Ubuntu:

```bash
cd /mnt/c/Users/moab/GitRepos/container-from-scratch
./scripts/setup-wsl.sh   # apt packages, git safe.directory, Go toolchain check
make rootfs              # debootstrap a minimal Ubuntu rootfs into /var/lib/runt/rootfs
```

Notes:

- `make rootfs` also shifts the rootfs's file ownership into the user-namespace ID range (uid 0 becomes 100000, and so on), so root inside the user namespace owns its files. If the range changes, run `make rootfs` again to re-shift; `runt run` refuses to start until the rootfs matches.
- `make build` sets `CGO_ENABLED=0`. Capabilities are per thread, and the Go call that drops them on every thread (`syscall.AllThreadsSyscall`) doesn't work in a cgo binary.
- Container devices are bind-mounted from the host's `/dev`, because a user namespace isn't allowed to create device nodes.
- Ubuntu's `golang-go` package may be older than `go.mod`; `GOTOOLCHAIN=auto` downloads Go 1.27 on first use.
- The rootfs lives on the WSL ext4 filesystem rather than under `/mnt/c`, because the Windows mount can't hold device nodes or Linux ownership.
- Files under `/mnt/c` are owned by root, so git inside WSL needs the repo in `safe.directory` (otherwise `go build` fails VCS stamping).
- `.gitattributes` forces LF line endings so the scripts still run in Linux with `core.autocrlf=true`.
- `.vscode/settings.json` sets `GOOS=linux` for gopls so `runtime_linux.go` gets full editor support on Windows.

## Hello world

```bash
make hello
```

## First container experiment

Run as root inside WSL:

```bash
make shell   # builds runt, then: sudo ./runt run --rootfs /var/lib/runt/rootfs -- /bin/bash
```

Or with any command:

```bash
make build
sudo ./runt run --rootfs /var/lib/runt/rootfs -- /bin/hostname   # prints "runt"
```

## User namespace ID range

Container IDs map onto a block of unprivileged host IDs read from `/etc/subuid` and `/etc/subgid`, where each line is `name:start:count`. `runt` uses the entry for the user who ran `sudo`, so with the usual Ubuntu entry `youruser:100000:65536`, container uid 0 is host uid 100000 and container uid 1000 is host uid 101000. If that user has no entry, it falls back to `100000:65536`.

To use another user's range, pass `--userns-user` and shift the rootfs to match:

```bash
make rootfs USERNS_USER=alice
make shell USERNS_USER=alice
# or: sudo ./runt run --rootfs /var/lib/runt/rootfs --userns-user alice -- /bin/bash
```

With `--userns-user`, a missing entry is an error rather than a fallback. Inside the container, `cat /proc/self/uid_map` shows the range in use.

## Capabilities

Before starting the command, `runt` drops every capability outside Docker's default set, from the bounding set as well, so a setuid program can't get them back. The 14 that remain:

`CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `KILL`, `SETGID`, `SETUID`, `SETPCAP`, `NET_BIND_SERVICE`, `NET_RAW`, `SYS_CHROOT`, `MKNOD`, `AUDIT_WRITE`, `SETFCAP`

Inside the container, `grep Cap /proc/self/status` shows `00000000a80425fb` for the permitted, effective, and bounding sets, the same value as a default Docker container.

See [ROADMAP.md](ROADMAP.md) for the remaining steps toward a Docker-like runtime.