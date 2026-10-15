# Roadmap

The steps between `runt` and a Docker-like container runtime. Each stage builds on the ones before it.

## Done

- [x] Mount a fresh `/proc` inside the container so `ps` and `top` work.
- [x] Switch from `chroot` to `pivot_root`, and detach the host's old root.
- [x] Make mounts private so mounts made inside the container don't leak to the host.
- [x] Minimal `/dev` on tmpfs: `null`, `zero`, `full`, `random`, `urandom`, `tty` (bind-mounted from the host), plus `/dev/pts`, `/dev/shm`, and the standard symlinks.
- [x] Network namespace (`CLONE_NEWNET`) with the loopback interface brought up.
- [x] IPC namespace (`CLONE_NEWIPC`).
- [x] User namespace (`CLONE_NEWUSER`): container root is an unprivileged host uid, and `make rootfs` shifts file ownership to match.
- [x] Cgroup namespace (`CLONE_NEWCGROUP`).
- [x] Mount `/sys` read-only, showing only the container's network namespace.
- [x] Mount `/dev/mqueue` for POSIX message queues in the IPC namespace.
- [x] User namespace ID range read from `/etc/subuid` and `/etc/subgid` (`--userns-user` picks whose entry), instead of fixed at 100000.
- [x] Drop Linux capabilities down to Docker's default set of 14.
- [x] seccomp filter that blocks about 60 dangerous syscalls, following Docker's default profile.
- [x] Mount sensitive paths read-only (`/proc/sys`, etc.) and hide others (`/proc/kcore`, etc.).
- [x] Set `no_new_privs` so programs can't gain privileges, for example through setuid binaries.

## Stage 2: Security

- [x] Optional: AppArmor or SELinux profiles. The AppArmor profile (`apparmor/runt-default`) and `--apparmor-profile` flag are tested on WSL2 with AppArmor turned on (`scripts/enable-apparmor-wsl.sh`). SELinux is not supported.

## Stage 3: Resource limits

- [x] cgroups v2 limits for CPU, memory, number of processes, and disk I/O (`--memory`, `--cpus`, `--pids-limit`, `--device-{read,write}-{bps,iops}`).

## Stage 4: Lifecycle

- [x] Give each container an ID and save its state under `/run/runt/<id>/`.
- [x] Commands: `create`, `start`, `ps`, `stop`, `kill`, `rm`.
- [x] Detached mode (`-d`) to run containers in the background, with a monitor process per container.
- [x] `exec` to run a command in a running container (using `setns`, through util-linux's `nsenter`, because Go can't join a user namespace).
- [x] Capture container output so `logs` can show it later.
- [x] Proper PID 1: forward signals and reap zombie processes. Init forwards every signal to the command and waits for every orphan the kernel hands it.
- [x] TTY support (`-it`) so `vim`, Ctrl+C, and similar work properly. Works for `run`, `run -dt` (logged), `create -t`, and `exec -it`. The pseudo-terminal is made inside the container, as in runc.
- [x] `attach`, to connect to a background container's terminal or input and output (and so `run -dit`, `create -i`, and `start -ai`). The monitor listens on a Unix socket in the container's state directory; Ctrl+P, Ctrl+Q detaches.

## Stage 5: Images

- [x] Layered filesystem with overlayfs: shared read-only image layers plus a writable layer per container. `--rootfs` is a single shared layer, too.
- [x] OCI image and runtime-config formats. `--image` runs an image from an OCI image layout (gzip or uncompressed layers), and `--bundle` runs an OCI runtime bundle's `config.json` (the subset in the README). `runt spec` writes one.
- [x] Pull images from a registry (Docker Hub, etc.): manifests, multi-platform indexes, layers, and token or basic authentication, with credentials from `runt login` or Docker's `config.json`. `run` pulls a missing image itself.
- [x] Local image store with `images` and `rmi` commands. The store is an OCI image layout under `/var/lib/runt/images`; `rmi` also removes blobs and unpacked layers nothing needs.
- [x] Apply image config: default command, environment variables, working directory, user, with `--entrypoint`, `-e`, `-w`, and `-u` to override them. Users are looked up in the container's own `/etc/passwd` and `/etc/group`.

## Stage 6: Networking

- [ ] Bridge network with a veth pair per container.
- [ ] IP address management (assign each container an IP).
- [ ] Outbound internet through NAT (iptables or nftables).
- [ ] Port publishing (`-p 8080:80`).
- [ ] DNS: write `/etc/resolv.conf` and `/etc/hosts` inside the container.
- [ ] Container-to-container name resolution on user-defined networks.

## Stage 7: Storage

- [ ] Bind mounts (`-v /host/path:/container/path`).
- [ ] Named volumes that survive after the container is removed.
- [ ] tmpfs mounts (in-memory folders).

## Stage 8: Building images

- [ ] Dockerfile parser (`FROM`, `RUN`, `COPY`, `CMD`, `ENV`, ...).
- [ ] Builder that runs each step in a temporary container and saves the result as a layer.
- [ ] Build cache to skip unchanged steps.
- [ ] Push images to a registry.

## Stage 9: Daemon and API

- [ ] Background daemon (like `dockerd`) that manages all containers.
- [ ] REST API, with the `runt` command acting as a client.
- [ ] Restart policies (`--restart always`).
- [ ] Health checks.

## Stage 10: Extras

- [ ] Compose: multi-container apps from a YAML file.
- [ ] Rootless mode (no `sudo` needed).
- [ ] `events`, `stats`, and `inspect` commands.
- [ ] `cp`, `commit`, and `export`.

## Suggested next steps

With images pulled from registries and run with their own configuration, most images work as they do in Docker, except those that need the network: containers only have a loopback interface. The bridge network with NAT (Stage 6) is what gives them internet access, and port publishing makes servers such as nginx reachable from the host. Bind mounts and volumes (Stage 7) are small by comparison and make containers useful for real work. For building images, `buildah` can fill the gap until Stage 8, writing OCI layouts that `--image` runs.
