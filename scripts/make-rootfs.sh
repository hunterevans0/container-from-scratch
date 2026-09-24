#!/usr/bin/env bash
# Build a minimal Ubuntu root filesystem for runt with debootstrap.
# Usage: sudo scripts/make-rootfs.sh [TARGET_DIR] [SUITE]
set -euo pipefail

target="${1:-/var/lib/runt/rootfs}"
suite="${2:-$(. /etc/os-release && echo "$VERSION_CODENAME")}"

if [[ $EUID -ne 0 ]]; then
    echo "make-rootfs.sh must run as root" >&2
    exit 1
fi

if [[ -x "$target/bin/sh" ]]; then
    echo "rootfs already exists at $target"
    exit 0
fi

mkdir -p "$target"
debootstrap --variant=minbase "$suite" "$target" http://archive.ubuntu.com/ubuntu
echo "rootfs ready at $target"
