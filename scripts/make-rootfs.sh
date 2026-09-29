#!/usr/bin/env bash
# Build a minimal Ubuntu root filesystem for runt with debootstrap, then shift
# its ownership into the user-namespace ID range.
# Usage: sudo scripts/make-rootfs.sh [TARGET_DIR] [SUITE]
set -euo pipefail

target="${1:-/var/lib/runt/rootfs}"
suite="${2:-$(. /etc/os-release && echo "$VERSION_CODENAME")}"

# Container root is host UID/GID 100000. Must match idMapBase in
# internal/runtime/runtime_linux.go.
id_base=100000

if [[ $EUID -ne 0 ]]; then
    echo "make-rootfs.sh must run as root" >&2
    exit 1
fi

if [[ -x "$target/bin/sh" ]]; then
    echo "rootfs already exists at $target"
else
    mkdir -p "$target"
    debootstrap --variant=minbase "$suite" "$target" http://archive.ubuntu.com/ubuntu
fi

if [[ "$(stat -c %u "$target")" -eq "$id_base" ]]; then
    echo "rootfs ownership already shifted"
else
    echo "shifting rootfs ownership by $id_base for the user namespace..."
    # chown clears setuid/setgid bits, so record them and restore afterwards.
    setid_list="$(mktemp)"
    find "$target" -xdev -type f -perm /6000 -printf '%m\t%p\n' > "$setid_list"

    for uid in $(find "$target" -xdev -printf '%U\n' | sort -un); do
        chown -R -h --from="$uid" "$((id_base + uid))" "$target"
    done
    for gid in $(find "$target" -xdev -printf '%G\n' | sort -un); do
        chown -R -h --from=":$gid" ":$((id_base + gid))" "$target"
    done

    while IFS=$'\t' read -r mode path; do
        chmod "$mode" "$path"
    done < "$setid_list"
    rm -f "$setid_list"
fi

echo "rootfs ready at $target"
