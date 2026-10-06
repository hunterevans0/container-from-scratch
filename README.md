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
- a seccomp filter that blocks about 60 dangerous syscalls
- `no_new_privs`, so setuid programs can't raise privileges
- sensitive `/proc` and `/sys` paths hidden or made read-only
- a cgroup v2 cgroup per container, with optional memory, CPU, process, and disk I/O limits
- an ID per container, with its state saved under `/run/runt/<id>/`

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

## Debugging with breakpoints

`runt run` starts two processes: the parent (`Run`) and the container's init (`Init`), which is the same binary run again inside the new namespaces. A debugger attached to one doesn't stop in the other, so there are two VS Code debug configurations. Each runs Delve as root in WSL, and VS Code on Windows connects to it on localhost.

1. Install Delve in WSL: `go install github.com/go-delve/delve/cmd/dlv@latest`.
2. Set breakpoints, then pick a configuration in the Run and Debug view:
   - **runt: debug parent (Run)** runs `make debug` (port 2345).
   - **runt: debug container init (Init)** runs `make debug-init`, which starts the container with `--debug-init`. Init prints its host PID and waits before doing any setup. At the same time, `make debug-attach-init` waits for that process and attaches Delve to it (port 2346).
3. Enter your sudo password in the task terminal. The container's shell also runs in that terminal.

Without VS Code, run the same make targets and connect any Delve client, for example `dlv connect 127.0.0.1:2345`. The debug binary, `runt-debug`, is built without optimizations or inlining so all variables can be inspected. Delve listens only on localhost, but with `--only-same-user=false`, because connections forwarded from Windows don't come from root. That means any local user can connect to the root debugger while it runs.

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

## Seccomp filter

Capabilities don't cover everything. `unshare(CLONE_NEWUSER)`, for example, needs no privilege at all and hands back a full set of capabilities inside the new namespace. So the last thing `runt` does before starting the command is install a seccomp filter: a small BPF program the kernel runs on every syscall.

The filter is a deny list that follows Docker's default profile. Everything is allowed except:

- about 60 syscalls that fail with `EPERM`: mounting (`mount`, `umount2`, `pivot_root`, `fsopen`, ...), namespaces (`unshare`, `setns`), kernel modules and `kexec`, `reboot`, setting the clock or hostname, the kernel keyring, `bpf`, `perf_event_open`, `io_uring`, `userfaultfd`, and others
- `clone` when it asks for a new namespace
- `clone3`, which fails with `ENOSYS` so glibc falls back to `clone` (a filter can't read `clone3`'s flags)
- `personality`, apart from the handful of values Docker allows; this stops `setarch -R` from switching off ASLR
- any syscall made through another ABI (x32, or 32-bit `int 0x80`), where the numbers differ

The lists live in [seccomp_linux_amd64.go](internal/runtime/seccomp_linux_amd64.go) and [seccomp_linux_arm64.go](internal/runtime/seccomp_linux_arm64.go); other architectures aren't supported. The filter is written by hand rather than with libseccomp, to keep the project free of dependencies and cgo.

Docker's real profile is an allow list, which also blocks syscalls added to the kernel in the future. A deny list is shorter and easier to read, but each new dangerous syscall has to be added to it.

Inside the container, `grep Seccomp /proc/self/status` shows mode `2` (filter), and `unshare -U true` fails with "Operation not permitted".

## no_new_privs

`runt` sets the `no_new_privs` flag, which makes the kernel ignore setuid and setgid bits and file capabilities when a program is executed. A process in the container can never have more privileges than its parent. Root can still switch to another user, but a non-root user can't get back to root with `su` or `sudo`. Docker leaves this off unless you pass `--security-opt no-new-privileges`.

Inside the container, `grep NoNewPrivs /proc/self/status` shows `1`.

## Hidden and read-only paths

Some files in `/proc` and `/sys` describe the host or control the kernel, and they aren't namespaced. `runt` uses Docker's default lists:

- Hidden: `/proc/asound`, `/proc/acpi`, `/proc/interrupts`, `/proc/kcore`, `/proc/keys`, `/proc/latency_stats`, `/proc/timer_list`, `/proc/timer_stats`, `/proc/sched_debug`, `/proc/scsi`, `/sys/firmware`, `/sys/devices/virtual/powercap`. Files are covered with `/dev/null` and directories with an empty read-only tmpfs.
- Read-only: `/proc/bus`, `/proc/fs`, `/proc/irq`, `/proc/sys`, `/proc/sysrq-trigger`.

The container can't undo these mounts, because unmounting needs `CAP_SYS_ADMIN`.

## Resource limits

Each container gets its own cgroup, `/sys/fs/cgroup/runt/<id>`. The container is cloned straight into it, so limits apply from its first instruction. The flags follow `docker run`:

| Flag | Example | Writes |
| --- | --- | --- |
| `--memory SIZE` | `--memory 512m` | `memory.max`, and `memory.swap.max` to the same amount, as Docker does by default |
| `--cpus N` | `--cpus 1.5` | `cpu.max` (`150000 100000`: 150ms of CPU time every 100ms) |
| `--pids-limit N` | `--pids-limit 100` | `pids.max` |
| `--device-read-bps PATH:RATE` | `--device-read-bps /dev/sdd:10mb` | `io.max` `rbps` |
| `--device-write-bps PATH:RATE` | `--device-write-bps /dev/sdd:10mb` | `io.max` `wbps` |
| `--device-read-iops PATH:N` | `--device-read-iops /dev/sdd:1000` | `io.max` `riops` |
| `--device-write-iops PATH:N` | `--device-write-iops /dev/sdd:1000` | `io.max` `wiops` |

Sizes take `k`, `m`, `g`, or `t` (powers of 1024), with an optional `b`. The device flags can be repeated, and take whole disks rather than partitions. `lsblk -d` lists them; in WSL the root filesystem is usually on `/dev/sdd`. The disk I/O limits only slow I/O that reaches the disk, so test them with direct I/O, for example `dd if=/dev/zero of=/root/test bs=1M count=8 oflag=direct`.

```bash
sudo ./runt run --rootfs /var/lib/runt/rootfs --memory 64m --cpus 0.5 --pids-limit 50 -- /bin/bash
```

`pids.max` counts threads as well as processes, and runt's own init uses several, so very small limits stop the container from starting.

Inside the container, its cgroup is mounted read-only at `/sys/fs/cgroup`, so `cat /sys/fs/cgroup/memory.max` shows the limit and `memory.current` the usage. Read-only stops the container from raising its own limits. When the container exits, runt kills anything left in the cgroup and removes it.

## Container state

Each container gets a random 12-character hex ID and a directory `/run/runt/<id>/` holding `state.json`:

```bash
sudo ls /run/runt
sudo cat /run/runt/<id>/state.json
```

The state records the rootfs, command, limits, cgroup, and timestamps, and moves through three statuses: `created` before the container starts, `running` with the host PID of its init, and `stopped` with its exit code. `/run` is a tmpfs, so the state is gone after a reboot. Stopped containers stay listed until then, because there is no `rm` command yet.

`runt run` exits with the container command's exit code, like `docker run`, or 128 plus the signal number if a signal killed it. It passes SIGINT, SIGTERM, SIGHUP, and SIGQUIT on to the container, so it can record the exit and clean up the cgroup.

## AppArmor (optional)

`apparmor/runt-default` is an AppArmor profile modelled on Docker's `docker-default`. With `--apparmor-profile runt-default`, the container's command runs confined by it. runt's init process stays unconfined, because it still has mounts to make, and the profile takes effect when it starts the command.

WSL2's kernel has AppArmor built in, but boots with SELinux as its security module and no SELinux policy, so AppArmor is off. To turn it on:

1. On Windows, add this to `%UserProfile%\.wslconfig`, then run `wsl --shutdown`:

   ```ini
   [wsl2]
   kernelCommandLine = lsm=landlock,lockdown,yama,integrity,apparmor
   ```

2. In WSL, run `scripts/enable-apparmor-wsl.sh`. It mounts securityfs and adds it to `/etc/fstab`, because WSL's boot doesn't mount it and AppArmor can't load profiles without it. It then loads the system's profiles and runt's.

After a restart, Ubuntu's `apparmor` service loads the system's profiles again, but not runt's. Load it with `make apparmor`:

```bash
make apparmor   # loads apparmor/runt-default into the kernel
sudo ./runt run --rootfs /var/lib/runt/rootfs --apparmor-profile runt-default -- /bin/bash
```

Inside, `cat /proc/self/attr/current` shows `runt-default (enforce)`. Most of what the profile denies is already blocked by dropped capabilities and read-only mounts, so it is a second layer. One thing only it blocks: confined processes can't inspect unconfined ones, so `cat /proc/1/maps` (runt's init) fails with "Permission denied", while it works without the profile. Without the flag, no profile is applied.

To turn AppArmor off again, remove the `kernelCommandLine` line and run `wsl --shutdown`. The kernel setting affects every WSL distro, including Docker Desktop's, because they share one kernel.

SELinux isn't supported. WSL's kernel can run it, but that needs a policy loaded and the filesystem labelled, which Ubuntu doesn't do.

See [ROADMAP.md](ROADMAP.md) for the remaining steps toward a Docker-like runtime.
