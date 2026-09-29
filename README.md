# runt

`runt` is a small container runtime built from scratch in Go. It demonstrates the core Linux process-isolation path:

- `CLONE_NEWUSER` for a user namespace: root inside the container is unprivileged host uid 100000
- `CLONE_NEWUTS` for an isolated hostname
- `CLONE_NEWPID` for a separate process-ID namespace
- `CLONE_NEWNS` for a separate mount namespace, made private so mounts don't leak to the host
- `CLONE_NEWNET` for a separate network with only a loopback interface (no internet access yet)
- `CLONE_NEWIPC` for separate shared memory and message queues
- `CLONE_NEWCGROUP` so the container sees its own cgroup as the root (`0::/`)
- `pivot_root` for a filesystem boundary, with the host's root detached
- a fresh `/proc`, so `ps` and `top` only see the container's processes
- a minimal `/dev` on tmpfs with `null`, `zero`, `full`, `random`, `urandom`, `tty`, a private `/dev/pts`, and `/dev/shm`

This is educational code, not a production container runtime. It currently requires Linux and root privileges for `run`.

## Setup on Windows

Go 1.27 is installed on the Windows host. Container execution needs Linux, so development runs inside Ubuntu WSL2. From Ubuntu:

```bash
cd /mnt/c/Users/moab/GitRepos/container-from-scratch
./scripts/setup-wsl.sh   # apt packages, git safe.directory, Go toolchain check
make rootfs              # debootstrap a minimal Ubuntu rootfs into /var/lib/runt/rootfs
```

Notes:

- `make rootfs` also shifts the rootfs's file ownership by 100000 (uid 0 becomes 100000, and so on), so root inside the user namespace owns its files. If you built the rootfs before user namespaces were added, run `make rootfs` again; `runt run` refuses to start until you do.
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

See [ROADMAP.md](ROADMAP.md) for the remaining steps toward a Docker-like runtime.