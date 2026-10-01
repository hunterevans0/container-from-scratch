#!/usr/bin/env bash
# Build a minimal Ubuntu root filesystem for runt with debootstrap, then shift
# its ownership into the user-namespace ID range.
# Usage: sudo [USERNS_USER=NAME] scripts/make-rootfs.sh [TARGET_DIR] [SUITE]
set -euo pipefail

target="${1:-/var/lib/runt/rootfs}"
suite="${2:-$(. /etc/os-release && echo "$VERSION_CODENAME")}"

# The user whose /etc/subuid and /etc/subgid ranges the container maps onto.
# This lookup must match internal/runtime/subid.go: the user asked for, else
# whoever ran sudo, else the current user; and 100000 if they have no entry.
userns_user="${USERNS_USER:-${SUDO_USER:-${USER:-}}}"
default_base=100000

# subid_base FILE: print the start of userns_user's first range in FILE.
subid_base() {
    local base=""
    if [[ -r "$1" ]]; then
        base="$(awk -F: -v user="$userns_user" '$1 == user && NF == 3 { print $2; exit }' "$1")"
    fi
    if [[ -z "$base" ]]; then
        if [[ -n "${USERNS_USER:-}" ]]; then
            echo "no entry for \"$userns_user\" in $1" >&2
            return 1
        fi
        base="$default_base"
    fi
    echo "$base"
}

# shift_ids FORMAT PREFIX DELTA: add DELTA to every owner in the rootfs.
# FORMAT/PREFIX are "%U" "" for users and "%G" ":" for groups.
shift_ids() {
    local format="$1" prefix="$2" delta="$3" order="-n" id
    # Go highest first when shifting up and lowest first when shifting down,
    # so an ID never lands on one that is still waiting its turn.
    if (( delta > 0 )); then
        order="-nr"
    fi
    for id in $(find "$target" -xdev -printf "$format\n" | sort -u "$order"); do
        chown -R -h --from="$prefix$id" "$prefix$((id + delta))" "$target"
    done
}

if [[ $EUID -ne 0 ]]; then
    echo "make-rootfs.sh must run as root" >&2
    exit 1
fi

uid_base="$(subid_base /etc/subuid)"
gid_base="$(subid_base /etc/subgid)"

if [[ -x "$target/bin/sh" ]]; then
    echo "rootfs already exists at $target"
else
    mkdir -p "$target"
    debootstrap --variant=minbase "$suite" "$target" http://archive.ubuntu.com/ubuntu
fi

# The rootfs directory belongs to container root, so its owner is the base
# the rootfs is shifted to now: 0 when fresh from debootstrap, or an older
# range if /etc/subuid has changed since.
uid_delta=$((uid_base - $(stat -c %u "$target")))
gid_delta=$((gid_base - $(stat -c %g "$target")))

if (( uid_delta == 0 && gid_delta == 0 )); then
    echo "rootfs ownership already shifted to $uid_base:$gid_base"
else
    echo "shifting rootfs ownership to $uid_base:$gid_base for the user namespace..."
    # chown clears setuid/setgid bits, so record them and restore afterwards.
    setid_list="$(mktemp)"
    find "$target" -xdev -type f -perm /6000 -printf '%m\t%p\n' > "$setid_list"

    if (( uid_delta != 0 )); then
        shift_ids '%U' '' "$uid_delta"
    fi
    if (( gid_delta != 0 )); then
        shift_ids '%G' ':' "$gid_delta"
    fi

    while IFS=$'\t' read -r mode path; do
        chmod "$mode" "$path"
    done < "$setid_list"
    rm -f "$setid_list"
fi

echo "rootfs ready at $target"
