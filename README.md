# runt

`runt` is a small container runtime built from scratch in Go. It demonstrates the core Linux process-isolation path:

- `CLONE_NEWUSER` for a user namespace: root inside the container is an unprivileged host uid, taken from `/etc/subuid` and `/etc/subgid` (usually 100000)
- `CLONE_NEWUTS` for an isolated hostname
- `CLONE_NEWPID` for a separate process-ID namespace
- `CLONE_NEWNS` for a separate mount namespace, made private so mounts don't leak to the host
- `CLONE_NEWNET` for a separate network: a veth pair to a bridge on the host, an address of the container's own, and NAT to the internet (or, with `--network none`, only a loopback interface)
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
- an ID per container, with its state and output saved under `/run/runt/<id>/`
- `create`, `start`, `ps`, `stop`, `kill`, `rm`, `exec`, `logs`, and `attach` commands, and detached containers (`run -d`, `run -dit`)
- an init process that, as PID 1 should, passes signals on to the command and reaps orphaned processes
- a pseudo-terminal of the container's own for interactive programs (`run -it`, `exec -it`)
- an overlay filesystem per container: shared read-only layers underneath, and a writable layer of its own on top
- OCI image layouts (`--image`) and OCI runtime bundles (`--bundle`, with `runt spec` to write a `config.json`)
- `pull` from Docker Hub and other registries (with `login`), a local image store with `images` and `rmi`, and each image's default command, environment, working directory, and user
- published ports (`-p 8080:80`), an `/etc/resolv.conf` and `/etc/hosts` in every container, and networks of your own (`runt network create`), apart from each other, whose containers find each other by name

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
make shell   # builds runt, then: sudo ./runt run -it --rootfs /var/lib/runt/rootfs -- /bin/bash
```

Or with any command:

```bash
make build
sudo ./runt run --rootfs /var/lib/runt/rootfs -- /bin/hostname   # prints "runt"
```

The rootfs itself never changes: whatever the shell writes or deletes goes to a writable layer of the container's own (see [Filesystem layers](#filesystem-layers)), and is gone after `runt rm`.

## Integration tests

`make test` only runs tests that don't start containers (the layer-unpacking tests also skip themselves unless run as root). `make integration` builds `runt` and runs the tests in [integration/](integration/), which start real containers with `sudo`. They check that containers can't reach each other (one tries to see the other's processes, kill it by its host PID, and move itself into its cgroup) and don't see each other's files. They also cover init reaping orphans, terminals for `run -it`, `run -dt`, and `exec -it`, signals reaching `exec`'d commands, `attach` (and detaching from it) and `start -ai`, an OCI image built from the rootfs, an image's configuration and the options that override it, a bundle made with `runt spec`, the bridge network, published ports, `/etc/resolv.conf` and `/etc/hosts`, and networks made with `runt network create`. One test serves an image from a registry of its own on localhost, which wants a password, and logs in, pulls, runs, and removes it. They need the rootfs from `make rootfs`, but not the internet: the network tests stand a network namespace in for it.

`make features` runs only the feature checks, in [integration/features_test.go](integration/features_test.go) and [integration/network_test.go](integration/network_test.go): a test for each image, filesystem, and network feature (`Layers`, `OCIFormats`, `Registry`, `ImageStore`, `ImageConfig`, `Network`, `Ports`, `DNS`, `UserNetworks`), with a subtest per check, each printing a line saying what it shows. Checks that pass print nothing else; one that fails is marked, with the details after its feature. `make features FEATURE=Registry` runs one of them, and `FEATURE=Registry/BasicAuth` one check.

## Debugging with breakpoints

`runt run` starts two processes: the parent (`Run`) and the container's init (`Init`), which is the same binary run again inside the new namespaces. A debugger attached to one doesn't stop in the other, so there are two VS Code debug configurations. Each runs Delve as root in WSL, and VS Code on Windows connects to it on localhost.

1. Install Delve in WSL: `go install github.com/go-delve/delve/cmd/dlv@latest`.
2. Set breakpoints, then pick a configuration in the Run and Debug view:
   - **runt: debug parent (Run)** runs `make debug` (port 2345).
   - **runt: debug container init (Init)** runs `make debug-init`, which starts the container with `--debug-init`. Init prints its host PID and waits before doing any setup. At the same time, `make debug-attach-init` waits for that process and attaches Delve to it (port 2346).
   - **runt: debug integration tests** runs `make debug-integration` (port 2347), which runs the tests in [integration/](integration/) under Delve. Breakpoints hit in the test code, not in the `runt` it starts.
3. Enter your sudo password in the task terminal. The container's shell also runs in that terminal.

Without VS Code, run the same make targets and connect any Delve client, for example `dlv connect 127.0.0.1:2345`. The debug binary, `runt-debug`, is built without optimizations or inlining so all variables can be inspected. Delve listens only on localhost, but with `--only-same-user=false`, because connections forwarded from Windows don't come from root. That means any local user can connect to the root debugger while it runs.

## User namespace ID range

Container IDs map onto a block of unprivileged host IDs read from `/etc/subuid` and `/etc/subgid`, where each line is `name:start:count`. `runt` uses the entry for the user who ran `sudo`, so with the usual Ubuntu entry `youruser:100000:65536`, container uid 0 is host uid 100000 and container uid 1000 is host uid 101000. If that user has no entry, it falls back to `100000:65536`.

To use another user's range, pass `--userns-user` and shift the rootfs to match:

```bash
make rootfs USERNS_USER=alice
make shell USERNS_USER=alice
# or: sudo ./runt run -it --rootfs /var/lib/runt/rootfs --userns-user alice -- /bin/bash
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
sudo ./runt run -it --rootfs /var/lib/runt/rootfs --memory 64m --cpus 0.5 --pids-limit 50 -- /bin/bash
```

`pids.max` counts threads as well as processes, and runt's own init uses several, so very small limits stop the container from starting.

Inside the container, its cgroup is mounted read-only at `/sys/fs/cgroup`, so `cat /sys/fs/cgroup/memory.max` shows the limit and `memory.current` the usage. Read-only stops the container from raising its own limits. When the container exits, runt kills anything left in the cgroup and removes it.

## Networking

By default, a container is on the bridge network, which works like Docker's default network: it has an address of its own, reaches other containers and the host directly, and the internet through the host. `-p` publishes its ports on the host. On a network made with `runt network create`, containers find each other by name.

```bash
sudo ./runt run -it --rootfs /var/lib/runt/rootfs -- /bin/bash
hostname -I                  # inside: 10.88.0.2
getent hosts example.com     # DNS, with the host's servers in /etc/resolv.conf

sudo ./runt run -d --name web -p 8080:80 --image nginx
curl http://localhost:8080/  # on the host, or at the host's address from another machine

sudo ./runt network create mynet
sudo ./runt run -d --name db --network mynet --image redis
sudo ./runt run -it --network mynet --image redis -- redis-cli -h db ping
```

**The bridge.** The host has a bridge, `runt0`: a virtual switch, with the address `10.88.0.1/16`. Each container gets a veth pair, two virtual interfaces joined like the ends of a cable. One end is `eth0` in the container's network namespace, and the other, `veth` followed by the first 8 characters of the container's ID, is plugged into the bridge (`ip link show master runt0` lists them). `runt` makes the pair once init has been cloned, with the container's end created straight in init's network namespace, and only then sends init its configuration. Init gives `eth0` its address, brings it up, and makes `10.88.0.1` its default route, while it still has `CAP_NET_ADMIN` in its own namespace. When the container stops, `runt` deletes the pair, which the kernel would also do once the namespace is gone.

**Addresses.** Each container gets the lowest free address in `10.88.0.0/16` (Podman's default range; Docker's is `172.17.0.0/16`), which `state.json` records as `ipAddress` while it runs. Its MAC address is made from it, as Docker does (`02:42:0a:58:00:02` for `10.88.0.2`), so a container that gets an address another one had also gets its MAC address, and ARP caches stay right. The bridge's MAC address is fixed the same way; otherwise a bridge takes the lowest MAC address of whatever is plugged into it, which changes as containers come and go.

An address in use has a file, `/run/runt-net/<network>/<address>` (the default network is `bridge`), holding the container's ID, which the process supervising the container keeps locked with `flock`. A new container takes the first address whose file nobody has locked, so an address whose monitor was killed before it could clean up is free again: the kernel dropped the lock when the process died. A new file is locked before it appears under the address's name (it's written under another name and then hard-linked), so two containers starting at once can't both take the same address. CNI's `host-local` plugin, which Podman uses, keeps a file per address the same way.

**NAT.** For the internet, the host passes containers' packets on to its own network (`net.ipv4.ip_forward`, which `runt` turns on, as Docker does), and these iptables rules let them through. Each network has its own, with its own subnet and bridge; these are the default network's:

| Table and chain | Rule | Why |
| --- | --- | --- |
| `nat POSTROUTING` | `-s 10.88.0.0/16 ! -o runt0 -j MASQUERADE` | Packets leaving the host get its address as their source, so replies come back to it. Connection tracking remembers each change and undoes it on the replies. |
| `filter FORWARD` | `-i runt0 ! -o runt+ -j ACCEPT` | What containers send beyond `runt`'s bridges is let through, even where the firewall drops forwarded packets by default, as Docker and ufw set it up. `runt+` matches every bridge `runt` makes. |
| `filter FORWARD` | `-i runt0 -o runt0 -j ACCEPT` | So is what they send each other, which the firewall sees only if `br_netfilter` is loaded (Docker loads it). |
| `filter FORWARD` | `-o runt0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT` | So are the replies. |
| `filter FORWARD` | `-o runt0 -m conntrack --ctstate DNAT -j ACCEPT` | And connections to [published ports](#publishing-ports). |

All networks share one more rule, `filter FORWARD -i runt+ -o runt+ -j DROP`: nothing else passes from one network to another. It goes in before any network's rules, which are each put first in their chain, so they come ahead of it.

The first container that needs them sets up a network's bridge, its rules, and forwarding, and they stay, as Docker leaves `docker0`. `runt network rm` removes a network's own. To remove the default network's, run `sudo ip link del runt0`, and `sudo iptables -D` for each rule (with `-t nat` for the first).

`runt` changes the network configuration with rtnetlink, the kernel interface `ip` uses. Go's standard library can't, and `runt` needs only a few requests, so [netlink_linux.go](internal/runtime/netlink_linux.go) builds them by hand, as the seccomp filter is.

`--network none` gives a container only a loopback interface, and an `/etc/hosts` with only `localhost`.

### Publishing ports

`-p 8080:80` publishes the container's port 80 at the host's port 8080, as `docker run -p` does. The forms are Docker's:

- `-p 8080:80` on all of the host's addresses
- `-p 127.0.0.1:8080:80` on only one of them
- `-p 53:53/udp` for UDP
- `-p 80`, or `-p 127.0.0.1::80`, at a free port the system picks

`runt ps` and `state.json` show which port was picked. Port ranges and IPv6 aren't supported.

Connections from other machines go through a DNAT rule. It lives in a chain of `runt`'s own, `RUNT` in the nat table, which `PREROUTING` jumps to for packets addressed to the host. `OUTPUT` jumps to it too, for the host's own connections to its non-loopback addresses:

```
-A RUNT ! -i runt+ -p tcp -m tcp --dport 8080 -j DNAT --to-destination 10.88.0.2:80
```

The kernel rewrites the destination as the packets arrive, and routes them on through the bridge. Connection tracking undoes the rewrite on the replies, and the container sees the client's own address.

Two kinds of connection can't go that way:

- The host's own, to `127.0.0.1:8080`. The kernel won't route a packet from a loopback address out of another interface.
- A container's, to the host's address. The container it's rewritten to would answer straight through the bridge, from an address the client doesn't expect.

Those go to a proxy instead, like Docker's `docker-proxy`. The process supervising the container listens on the host port and relays each connection, or for UDP each client's datagrams, to the container. The container sees them come from the bridge's address.

Listening also holds the port. A second container, or any other program, that tries to publish it fails with "address already in use". When the container stops, its rules go and its ports are free again. A monitor that's killed can't remove its rules, but its sockets close with it, so whoever next holds the port, or gets the address, removes any rules left for it.

### DNS

Every container gets an `/etc/resolv.conf` and an `/etc/hosts` of `runt`'s, as with Docker, since images come without a `resolv.conf`:

- `/etc/hosts` has `localhost`, and the container's hostname at its address.
- `/etc/resolv.conf` has the host's DNS servers, search domains, and options, without the servers on the host's loopback interface, which a container can't reach.

On a host using systemd-resolved, `/etc/resolv.conf` names only its stub server, `127.0.0.53`. There `runt` reads the servers it really uses from `/run/systemd/resolve/resolv.conf`, as Docker does. With no server left, a container gets Google's, `8.8.8.8` and `8.8.4.4`, also as with Docker. In WSL, it gets WSL's own, `10.255.255.254`, which containers reach through the bridge.

The supervisor writes the files and init bind-mounts them over the container's own, after `pivot_root`, so a symlink there can only lead within the container. Init first writes them to a small tmpfs of their own, and detaches it once they're mounted, so they leave the image's layers alone. The container's writable layer changes only when the image lacks a file, or has a symlink that leads nowhere: then an empty file is made there to mount over. A bundle's root filesystem, which `runt` uses as it is, gets them only over files it already has. The container can still change them, even with a read-only root.

### User-defined networks

`runt network create NAME` makes a network with a bridge of its own, named `runt-` and the start of the network's ID. Its subnet is the one `--subnet` gives, or else the first free `/24` from `10.89.0.0/24` on (as Podman picks), avoiding the host's own networks. `runt network ls` lists networks. `runt network rm` removes a network, with its bridge and rules, once no container is on it. Networks are kept in `/var/lib/runt/networks/`, so they survive a reboot, and a network's bridge is made when a container first joins it, as `runt0` is.

Containers on such a network find each other by name or ID, alone or followed by the network's name (`db.mynet`). Their `/etc/resolv.conf` names a DNS server at `127.0.0.11`, the address of Docker's embedded server. It also sets `options ndots:0`, so `db` is looked up as it is first, not under each search domain.

The server is the process supervising the container. It makes a UDP and a TCP socket inside the container's network namespace, with `setns` on a thread of its own that ends once they exist, so it answers on the container's own loopback interface. It looks names up among the containers created or running on the same network, in their `state.json`. Every other question it passes on to the host's DNS servers. It does that from the host's network namespace, so servers on the host's loopback interface, such as systemd-resolved's, work too. [dns_linux.go](internal/runtime/dns_linux.go) speaks just enough DNS for this, by hand.

As with Docker, containers on the default network don't get the server, and find each other only by address. Containers on different networks can't reach each other at all (see the `DROP` rule above), though each still reaches the host, the internet, and published ports.

## Managing containers

Besides running a container in the foreground, `runt` can run containers in the background and manage them with Docker-like commands. Run them all with `sudo`.

```bash
sudo ./runt run -d --name web --rootfs /var/lib/runt/rootfs -- /bin/sh -c 'while :; do date; sleep 1; done'
sudo ./runt ps                 # running containers; -a includes stopped ones
sudo ./runt logs -f web        # follow its output; -t adds timestamps, --tail N shows the last N lines
sudo ./runt exec web ps -e     # run another command inside it
sudo ./runt exec -it web bash  # or an interactive shell
sudo ./runt stop web           # SIGTERM, then SIGKILL after 10 seconds (-t to change)
sudo ./runt rm web

sudo ./runt run -dit --name box --rootfs /var/lib/runt/rootfs -- /bin/bash
sudo ./runt attach box         # its terminal; Ctrl+P, Ctrl+Q detaches and leaves it running
```

| Command | What it does |
| --- | --- |
| `run [-d] [-i] [-t] [OPTIONS] -- CMD` | Create a container and run `CMD`. With `-d` it runs in the background and `runt` prints its ID. `-i` and `-t` are as in Docker (see [Terminals](#terminals)). |
| `create [-i] [-t] [OPTIONS] -- CMD` | Set a container up (namespaces, mounts, cgroup, security) without running `CMD` yet. |
| `start [-a] [-i] CONTAINER...` | Run the command of a created container. `-a` attaches to its output first and waits for it to exit, and `-i` also sends it `runt`'s input. |
| `attach [--no-stdin] CONTAINER` | Connect `runt`'s input and output to a running container (see [Attach](#attach)). |
| `ps [-a]` | List running and created containers, or all with `-a`. |
| `stop [-t SECONDS] CONTAINER...` | Send SIGTERM, then SIGKILL if it hasn't exited after the timeout. |
| `kill [-s SIGNAL] CONTAINER...` | Send a signal, SIGKILL by default. |
| `rm [-f] CONTAINER...` | Remove a stopped container's state, log, and writable layer. `-f` kills it first if it's running. |
| `exec [-i] [-t] CONTAINER CMD [ARG...]` | Run a command inside a running container. |
| `logs [-f] [-t] [--tail N] CONTAINER` | Show what the container wrote to stdout and stderr. |
| `spec [--bundle DIR] [--rootfs PATH]` | Write a starting `config.json` for an OCI runtime bundle. |
| `pull IMAGE` | Download an image from a registry into the image store (see [Images](#images)). |
| `images [-q] [--digests]` | List the images in the store. |
| `rmi [-f] IMAGE...` | Remove images from the store. |
| `login [-u USER] [--password-stdin] [REGISTRY]` / `logout [REGISTRY]` | Store or remove credentials for a registry, Docker Hub by default. |
| `network create [--subnet CIDR] NAME` / `network ls` / `network rm NAME...` | Make, list, or remove [networks](#user-defined-networks) whose containers find each other by name. |

`run` and `create` take the container's filesystem from one of `--rootfs PATH` (a root filesystem directory), `--image IMAGE` (an [image](#images) from the store, pulled if need be, or an OCI image layout), or `--bundle DIR` (an [OCI runtime bundle](#oci-runtime-bundles), whose `config.json` also gives the command). They also take `--network bridge|none|NAME` and `-p [[HOSTIP:]HOSTPORT:]PORT[/udp]` (repeatable; see [Networking](#networking)), `-e NAME=VALUE` (repeatable), `-w DIR`, and `-u USER[:GROUP]`, as `docker run` does, and with an image, `--entrypoint` (see [Image configuration](#image-configuration)).

`CONTAINER` is the name given with `--name`, the full ID, or enough of the start of the ID to be unique. Options go before the other arguments, as in `logs -f web`. Single-letter options combine, as in `-it` or `-dt`.

### Container IDs and state

Each container gets a random 12-character hex ID and a directory `/run/runt/<id>/`:

- `state.json`: the rootfs or image, command, limits, cgroup, network, address and published ports, timestamps, and status. The status moves from `created` (set up, with the host PID of its init) to `running` and then `stopped` (with the exit code).
- `container.log`: the container's output, one JSON object per line, in the format of Docker's `json-file` log driver.
- `start.fifo`: what a created container's init waits on (see below).
- `attach.sock`: the Unix socket `runt attach` connects to, while the container runs.
- `monitor.log`: problems the background monitor ran into, if any.

`/run` is a tmpfs, so all of this is gone after a reboot. The container's writable layer is in `/var/lib/runt/containers/<id>/` (see [Filesystem layers](#filesystem-layers)), which does survive a reboot. The next container `runt` starts removes layers whose state has gone.

### How it works

**A monitor per container.** Something has to wait for a container to exit, record its exit code, save its output, and remove its cgroup. In the foreground that's `runt run` itself. For `run -d` and `create`, `runt` starts a copy of itself with `--monitor`, in a session of its own so it isn't tied to the terminal, and returns once the container is set up. This is the job `containerd-shim` does for Docker and `conmon` does for Podman. If a monitor is killed, the container dies with it (through `PR_SET_PDEATHSIG`), rather than running on with nothing to record its exit.

**Create, then start.** Init does all of its setup, then reads one byte from a FIFO before running the command. `runt start` writes that byte, which is how runc implements the same split. `runt run` writes it as soon as init reports that it's set up, which is also when the container's terminal has its size.

**Init as PID 1.** The command isn't PID 1 of the container; runt's init is, and the command is its child. The kernel gives PID 1 two jobs that a program like `bash` or `nginx` doesn't expect. First, signals: PID 1 only gets the signals it has a handler for, so a plain `kill` or `docker stop` would do nothing to a command running as PID 1 without one. Init handles every signal and passes it on to the command. Second, orphans: when a process exits, the kernel hands its children to PID 1. Each one stays a zombie when it exits, holding on to its PID, until PID 1 waits for it. Init waits for every child it gets, not only the command, as [tini](https://github.com/krallin/tini) (`docker run --init`) does. When the command exits, init exits with its status, and the kernel kills whatever is left in the container.

**Signals.** `stop` and `kill` signal the container's init, which passes the signal on to the command. A container that was killed reports 128 plus the signal number, so `stop` usually gives 143 (SIGTERM) and `kill` gives 137 (SIGKILL). In the foreground, `runt run` passes SIGINT, SIGTERM, SIGHUP, and SIGQUIT on the same way, and `runt exec` passes them on to its command.

**No controlling terminal.** Init, and every `exec`'d command, starts in a session of its own, without a controlling terminal, as in runc. Otherwise a process in the container could open `/dev/tty` and reach the terminal `runt` was started from. It could then change the terminal's settings, or push keystrokes into its input with the `TIOCSTI` ioctl, which the shell would run on the host once `runt` exits. Terminal signals such as Ctrl+C reach the container through `runt`. For interactive programs, the container gets a terminal of its own (see [Terminals](#terminals)).

**Output.** A container's output always goes to its log. In the foreground it goes to `runt`'s own output as well.

**Exit codes.** `runt run` exits with the container command's exit code, like `docker run`. If the command can't be run it exits with 127 when it isn't found and 126 otherwise, as shells do, and `runt exec` does the same.

**exec and setns.** `exec` joins the container's namespaces with the `setns` system call, but a Go program can't make that call for a user or mount namespace. The kernel only allows it in a single-threaded process, and the Go runtime has already started several threads before `main` runs. runc gets around this with C code that runs before the Go runtime starts. `runt` uses `nsenter` from util-linux, started in the container's cgroup, to make the `setns` calls. `nsenter` then runs `runt` again inside the container, passed in as an open file (`/proc/self/fd/3`) because the binary isn't in the container's filesystem. That copy applies the same AppArmor profile, `no_new_privs`, capabilities, and seccomp filter as the container's own command, then runs the command. Exec'd processes are root inside the container, see only its processes and files, and count against its limits.

**Inherited files.** Init and `exec` close every file descriptor above 2 before the command runs, as runc does, so files open in whatever started `runt` don't leak into the container. In WSL, for example, `/dev/ptmx` descriptors are open in every shell.

**Output pipes owned by container root.** The pipes for the container's output (and input, with `-i`) are made by `runt`, as host root, and then given to container root, as containerd does. Programs open `/dev/stdout` and `/dev/stderr`, which lead through `/proc/self/fd` to the pipe itself, and opening a pipe that way checks its owner. nginx's log files, for example, are links to them.

## Terminals

`-i` and `-t` work as in Docker:

- `-i` connects `runt`'s standard input to the command. Without it, the command's input is empty.
- `-t` runs the command on a pseudo-terminal of its own, so it sees a terminal: `bash` prompts, `vim` draws the screen, `ls` uses colours.

Use both, as `-it`, for an interactive program:

```bash
sudo ./runt run -it --rootfs /var/lib/runt/rootfs -- /bin/bash
sudo ./runt exec -it web /bin/bash
```

With `-it`, `runt` puts its own terminal in raw mode, so every key goes straight to the container. Ctrl+C is a byte the container's terminal turns into SIGINT for whatever is running there, rather than a signal for `runt`. Shells get job control. The container's terminal starts at the size of `runt`'s, and `runt` passes on window resizes (SIGWINCH). `runt` gives the error "the input device is not a terminal" if `-it` is used without one, as Docker does.

A pseudo-terminal is a pair of devices: whatever the command writes to one end, the "slave", can be read from the other, the "master", and the other way round. As in runc, init makes the pair inside the container from the container's own `devpts` (`/dev/ptmx`), so the command sees `/dev/pts/0`, owned by container root, rather than one of the host's terminals. The slave also appears as `/dev/console`. Init sends the master out to `runt` over a Unix socket, passing the open file descriptor itself (`SCM_RIGHTS`). `exec -t` works the same way, with its own terminal.

`-t` works in the background too: `run -dt` (or `create -t`) gives the command a terminal of 80 by 24 characters, and its output goes to the log. A shell started that way keeps running, waiting for input that never comes, so `exec -it` can join it. With `-i` as well (`run -dit`), `runt attach` can type into it.

Terminal output is logged as `stdout`, carriage returns and escape sequences included, as Docker logs it.

## Attach

`runt attach CONTAINER` connects to a container running in the background, as `docker attach` does: what the container writes appears in `runt`'s output, and what `runt` reads goes to the container's input, if it was made with `-i`. Whatever supervises the container (its monitor, or `runt run` in the foreground) listens on a Unix socket, `/run/runt/<id>/attach.sock`. It sends the container's output to everyone connected, and passes what they send on to the container's terminal with `-t`, or else to a pipe for its input that it holds open for the purpose. Several `attach`es can connect at once.

```bash
sudo ./runt run -dit --name box --rootfs /var/lib/runt/rootfs -- /bin/bash
sudo ./runt attach box            # Ctrl+P, Ctrl+Q to detach
sudo ./runt create -i --name job --rootfs /var/lib/runt/rootfs -- /bin/sh -c 'read x; echo $x'
echo hello | sudo ./runt start -ai job
```

- Like `exec -it`, attaching to a container with a terminal puts `runt`'s terminal in raw mode, and the container's terminal takes its size (the last one to attach or resize wins).
- **Ctrl+P, Ctrl+Q** detaches and leaves the container running. Ctrl+P followed by anything else is sent on as it is. Without a terminal, Ctrl+C is a signal for `runt`, which passes it on to the container (`--sig-proxy=false` doesn't).
- `attach` doesn't replay what the container wrote before; `runt logs` has that. So a shell shows no prompt until a key is pressed, as with Docker.
- When the container exits, `attach` exits with its exit code. When `attach`'s own input ends, the container's input stays open for others, as in Docker.
- `start -a` attaches first and then starts the container, so none of its output is missed. `-i` (which implies `-a`) sends `runt`'s input too.

The two ends talk in frames: a byte for the type, a 4-byte length, and the data. `runt attach` sends input and terminal sizes, and receives stdout, stderr, and finally the exit code. A client that falls far behind is disconnected rather than slowing the container down.

## Filesystem layers

Each container's root filesystem is an [overlayfs](https://docs.kernel.org/filesystems/overlayfs.html) mount: one or more read-only layers underneath, shared with every other container that uses them, and an empty writable layer on top that belongs to this container. Reading a file finds it in the topmost layer that has it. Changing a file copies it up into the writable layer first, and deleting one leaves a "whiteout" there that hides it. The layers underneath never change, so containers started from the same rootfs don't see each other's changes, and the rootfs stays as `make rootfs` left it.

With `--rootfs`, the rootfs directory is the only layer underneath. With `--image`, it's the image's layers. Each container has a directory `/var/lib/runt/containers/<id>/` with:

- `upper/`: the writable layer.
- `work/`: scratch space overlayfs needs.
- `rootfs/`: where the overlay is mounted while the container runs. Init uses this as the root filesystem.

`runt` mounts the overlay as host root before it starts init, so the mount is visible on the host while the container runs (`mount | grep runt`). It's mounted `nodev`, so a device file in an image can't be used to reach the host's disks. After the container stops, `runt` unmounts it. The writable layer stays until `runt rm`, as Docker keeps a stopped container's changes. Inspect it in `upper/`, where deleted files show up as character devices with device number 0/0.

The container's directory is mode `0710` with group container root (host gid 100000, usually), because init runs as container root and has to reach `rootfs/`. Other host users can't look in.

## Images

```bash
sudo ./runt pull alpine:3.20
sudo ./runt images
sudo ./runt run -it --image alpine:3.20                 # the image's default command, /bin/sh
sudo ./runt run -d --name web --image nginx:alpine      # pulled first if it isn't in the store
sudo ./runt rmi alpine:3.20
```

`--image IMAGE` takes an image from `runt`'s image store, by name or ID, or an OCI image layout directory. A name is written as Docker writes it, `[REGISTRY/]REPOSITORY[:TAG][@DIGEST]`: `alpine` is `docker.io/library/alpine:latest`, and `ghcr.io/owner/app:1.0` is on GitHub's registry. If the image isn't in the store, `run` and `create` pull it first, as `docker run` does. A path, or a directory with an `oci-layout` file, is a layout instead (see [OCI image layouts](#oci-image-layouts)).

### Pulling

`runt pull` talks to the registry with the [OCI distribution API](https://github.com/opencontainers/distribution-spec), which Docker Hub, `ghcr.io`, and every other registry serve. It asks for the image's manifest, by tag or digest, accepting OCI and Docker manifests. If what comes back is an index, a manifest per platform, it follows the one for Linux on this machine's architecture. Then it downloads the image's configuration and each layer it doesn't have yet, checking each one against its digest, and moves them into the store.

Registries answer a request without credentials with `401` and a challenge saying how to authenticate. `Basic` means sending a user name and password with every request. `Bearer` names a token server to ask for a token that allows pulling from the repository, sending the user name and password if there are any. Docker Hub asks for a token even for public images, and hands one out without credentials. Blob requests are often redirected to a CDN; Go's HTTP client doesn't pass the token on to another host.

A registry on this machine (`localhost` or `127.0.0.1`, with any port) is reached over plain HTTP, as Docker does; any other over HTTPS.

### Logging in

```bash
sudo ./runt login -u myname            # Docker Hub; asks for the password
echo "$TOKEN" | sudo ./runt login -u myname --password-stdin ghcr.io
sudo ./runt logout ghcr.io
```

`runt` keeps credentials where Docker does, in `config.json` under `$DOCKER_CONFIG`, or `~/.docker`. Under `sudo` that's root's, so a `docker login` you ran as yourself isn't seen; run `sudo ./runt login`, or set `DOCKER_CONFIG`. `login` checks the credentials with the registry first, then stores them in the file's `auths`, base64-encoded but not encrypted, like `docker login` without a credential helper. When reading, `runt` also asks a credential helper (`docker-credential-NAME`) if `config.json` names one in `credHelpers` or `credsStore`.

### The image store

The store, `/var/lib/runt/images/`, is itself an OCI image layout: blobs under `blobs/sha256/`, and an `index.json` listing each image under its full name (`docker.io/library/alpine:3.20`) in the `io.containerd.image.name` annotation, as `docker save` writes it. Only this platform's manifest is kept. An image's ID is the digest of its configuration, as in Docker, and `images` shows the first 12 digits, which `run --image` and `rmi` also accept.

`runt rmi NAME` removes that name. The image itself is deleted once no name is left for it; removing it by ID removes all its names, which takes `-f` if there is more than one. An image a container was made from (`runt ps -a`) also takes `-f`; the container keeps its layers until it's removed. Then `rmi` deletes the blobs no image needs, and unpacked layers that neither an image in the store nor a container uses, including those of layout images no container uses any more (they're unpacked again when next run).

Several `runt` processes use the store at once, so a lock file, `/var/lib/runt/images.lock`, guards it. A pull downloads into a directory of its own under `/var/lib/runt/tmp/`, holding the lock shared, then takes it exclusively for the moment it takes to move the blobs in and update `index.json`. Starting a container from an image holds the lock shared until the container's state, which lists its layers, is saved. `rmi` holds it exclusively, so it never removes something that is being pulled, unpacked, or mounted.

### Image configuration

An image's configuration says how to run it, with the defaults a Dockerfile's `ENTRYPOINT`, `CMD`, `ENV`, `WORKDIR`, and `USER` set. `runt` applies them as `docker run` does:

| Setting | From the image | Overridden by |
| --- | --- | --- |
| Command | the entrypoint followed by the default command | the arguments after the options replace the default command; `--entrypoint` replaces the entrypoint and drops the default command (`--entrypoint ""` for none) |
| Environment | the image's, plus `PATH` if it doesn't set one, `TERM=xterm` with `-t`, and `HOME` for the user | `-e NAME=VALUE`, or `-e NAME` to take it from `runt`'s environment |
| Working directory | `WORKDIR`, or `/` | `-w DIR`; it's created if it doesn't exist |
| User | `USER` | `-u USER[:GROUP]` |

A user or group is a name or a numeric ID. Names are looked up in the container's own `/etc/passwd` and `/etc/group`, from inside the container, as runc does: a name means whatever the image says it means. A numeric ID needn't be listed. Without a group, the user's primary group is used, and the user also gets the groups that list them as a member. The files come from the image, so init only reads regular files, and only so much of them.

`-e`, `-w`, and `-u` work with `--rootfs` and `--bundle` too, on top of `runt`'s own environment and the bundle's settings.

### OCI image layouts

`--image LAYOUT[:TAG]` also runs an image from an [OCI image layout](https://github.com/opencontainers/image-spec/blob/main/image-layout.md): a directory with an `oci-layout` file, an `index.json` listing the images in it, and every manifest, configuration, and layer stored under `blobs/sha256/` by the digest of its content. `skopeo` and `buildah` write them (`skopeo copy docker://alpine:3.20 oci:alpine:3.20`), and so does `docker save` from Docker 25 on, once untarred.

```bash
sudo ./runt run -it --image ./alpine:3.20 -- /bin/sh
```

`runt` picks the image tagged `TAG` (the `org.opencontainers.image.ref.name` annotation), or the only image, or the one tagged `latest`. If it comes in several architectures, `runt` picks the Linux one for the machine it runs on.

### Unpacking layers

The first time an image is run, from the store or a layout, `runt` unpacks each layer into `/var/lib/runt/layers/<uid base>-<gid base>/<digest>/`, where every container using that layer shares it. Unpacking:

- checks every blob against its digest, and each layer's uncompressed content against the image configuration's `diff_ids`;
- takes uncompressed or gzipped layers (`zstd` isn't supported, since Go's standard library has no decompressor for it);
- shifts file ownership into the user-namespace range, as `make rootfs` does for the rootfs, which is why the store is per range;
- turns OCI whiteouts into overlayfs ones: `.wh.NAME` becomes a 0/0 character device `NAME`, and `.wh..wh..opq` marks its directory opaque with the `trusted.overlay.opaque` attribute;
- treats the archive as untrusted: names are cleaned of `..`, no entry may be written through a symbolic link, and hard links must point inside the layer;
- skips device files and extended attributes. The one extended attribute images use, `security.capability`, has no effect under `no_new_privs`.

`runt rmi` removes unpacked layers that are no longer needed (see [The image store](#the-image-store)).

## OCI runtime bundles

`--bundle DIR` runs an [OCI runtime bundle](https://github.com/opencontainers/runtime-spec/blob/main/bundle.md), the format runc runs: a directory with a `config.json` describing the container, and usually its root filesystem. `runt spec` writes a starting `config.json`, like `runc spec`:

```bash
mkdir mybundle
sudo ./runt spec --bundle mybundle --rootfs /var/lib/runt/rootfs
sudo ./runt run -i --bundle mybundle        # config.json says terminal: true, so this is like -it
```

As with runc, the container uses the bundle's root filesystem directory itself, not an overlay on top of it, so `runt spec` makes it read-only (`root.readonly`). `runt` reads these parts of `config.json`:

- `root.path` (relative to the bundle, or absolute) and `root.readonly`
- `process.args`, `env` (which replaces the environment instead of inheriting `runt`'s), `cwd`, `user` (`uid`, `gid`, `additionalGids`), `terminal` (as `-t`), and `apparmorProfile`
- `hostname`
- `linux.uidMappings` and `linux.gidMappings`, each a single range starting at container ID 0. Without them, `runt` uses `/etc/subuid` and `/etc/subgid` as usual, and `runt spec` writes yours in.
- `linux.resources`: `memory.limit`, `cpu.quota` and `cpu.period`, and `pids.limit`

Options on the command line, such as `--memory`, win over the file's. `runt` always creates every namespace and applies its own security settings (capabilities, seccomp, masked paths, `no_new_privs`), so it ignores the fields for those. It refuses a `config.json` that asks for something it can't do, rather than quietly running the container differently. That includes mounts other than its standard ones (`/proc`, `/dev`, `/sys`, and those under them), and namespaces given by path to join.

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
sudo ./runt run -it --rootfs /var/lib/runt/rootfs --apparmor-profile runt-default -- /bin/bash
```

Inside, `cat /proc/self/attr/current` shows `runt-default (enforce)`. Most of what the profile denies is already blocked by dropped capabilities and read-only mounts, so it is a second layer. One thing only it blocks: confined processes can't inspect unconfined ones, so `cat /proc/1/maps` (runt's init) fails with "Permission denied", while it works without the profile. Without the flag, no profile is applied.

To turn AppArmor off again, remove the `kernelCommandLine` line and run `wsl --shutdown`. The kernel setting affects every WSL distro, including Docker Desktop's, because they share one kernel.

SELinux isn't supported. WSL's kernel can run it, but that needs a policy loaded and the filesystem labelled, which Ubuntu doesn't do.

See [ROADMAP.md](ROADMAP.md) for the remaining steps toward a Docker-like runtime.
