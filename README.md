# runt

`runt` is a small container runtime built from scratch in Go. The first slice demonstrates the core Linux process-isolation path:

- `CLONE_NEWUTS` for an isolated hostname
- `CLONE_NEWPID` for a separate process-ID namespace
- `CLONE_NEWNS` for a separate mount namespace
- `chroot` for a filesystem boundary

This is educational code, not a production container runtime. It currently requires Linux and root privileges for `run`.

## Setup on Windows

Go 1.27 is installed on the Windows host. Container execution needs Linux, so development runs inside Ubuntu WSL2. From Ubuntu:

```bash
cd /mnt/c/Users/moab/GitRepos/container-from-scratch
./scripts/setup-wsl.sh   # apt packages, git safe.directory, Go toolchain check
make rootfs              # debootstrap a minimal Ubuntu rootfs into /var/lib/runt/rootfs
```

Notes:

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

`/proc` is not mounted inside the container yet, so tools like `ps` won't work there.

The next framework increments should add mounted `/proc`, capability dropping, cgroups v2, UID/GID namespaces, seccomp, a lifecycle state directory, and OCI image/config support.