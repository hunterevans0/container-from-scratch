#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

sudo apt-get update
sudo apt-get install -y build-essential ca-certificates debootstrap git iproute2 \
    libcap2-bin make uidmap util-linux golang-go

# Files under /mnt/c are owned by root, so git refuses to read the repository
# (and go build fails VCS stamping) unless it is marked safe.
if ! git config --global --get-all safe.directory | grep -qxF "$repo_dir"; then
    git config --global --add safe.directory "$repo_dir"
fi

# Ubuntu's golang-go may be older than go.mod; GOTOOLCHAIN=auto fetches the
# required toolchain on first use.
(cd "$repo_dir" && go version)

echo "WSL dependencies are ready. Next: make rootfs && make build && make shell"
