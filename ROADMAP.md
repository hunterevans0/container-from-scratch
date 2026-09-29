# Roadmap

The steps between `runt` and a Docker-like container runtime. Each stage builds on the ones before it.

## Done

- [x] Mount a fresh `/proc` inside the container so `ps` and `top` work.
- [x] Switch from `chroot` to `pivot_root`, and detach the host's old root.
- [x] Make mounts private so mounts made inside the container don't leak to the host.

## Stage 1: Finish isolation

All in `internal/runtime/runtime_linux.go`.

- [ ] Create basic device nodes: `/dev/null`, `/dev/zero`, `/dev/urandom`, `/dev/tty`, and so on.
- [ ] Network namespace (`CLONE_NEWNET`) so the container gets its own separate network.
- [ ] IPC namespace (`CLONE_NEWIPC`) so the container can't reach host shared memory or message queues.
- [ ] User namespace (`CLONE_NEWUSER`): root inside the container, an unprivileged user outside.
- [ ] Cgroup namespace (`CLONE_NEWCGROUP`) so the container can't see the host's cgroup tree.

## Stage 2: Security

- [ ] Drop Linux capabilities down to Docker's default safe set.
- [ ] seccomp filter to block dangerous syscalls (Docker blocks about 50 by default).
- [ ] Mount sensitive paths read-only (`/proc/sys`, etc.) and hide others (`/proc/kcore`, etc.).
- [ ] Set `no_new_privs` so programs can't gain privileges, for example through setuid binaries.
- [ ] Optional: AppArmor or SELinux profiles.

## Stage 3: Resource limits

- [ ] cgroups v2 limits for CPU, memory, number of processes, and disk I/O (`--memory`, `--cpus`, `--pids-limit`).

## Stage 4: Lifecycle

- [ ] Give each container an ID and save its state under `/run/runt/<id>/`.
- [ ] Commands: `create`, `start`, `ps`, `stop`, `kill`, `rm`.
- [ ] Detached mode (`-d`) to run containers in the background.
- [ ] `exec` to run a command in a running container (using `setns`).
- [ ] Capture container output so `logs` can show it later.
- [ ] Proper PID 1: forward signals and reap zombie processes.
- [ ] TTY support (`-it`) so `vim`, Ctrl+C, and similar work properly.

## Stage 5: Images

- [ ] Layered filesystem with overlayfs: shared read-only image layers plus a writable layer per container.
- [ ] OCI image and runtime-config formats.
- [ ] Pull images from a registry (Docker Hub, etc.): manifests, layers, authentication.
- [ ] Local image store with `images` and `rmi` commands.
- [ ] Apply image config: default command, environment variables, working directory, user.

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

Device nodes, capability dropping, cgroups v2 memory/PID limits, and a state directory with an ID per container give the biggest improvement for the least work. After OCI support is in, existing tools can fill gaps: `skopeo` to pull images, `buildah` to build them.
