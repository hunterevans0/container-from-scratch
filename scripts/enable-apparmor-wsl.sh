#!/usr/bin/env bash
# Turn on AppArmor inside WSL2, for runt's --apparmor-profile.
#
# WSL's kernel has AppArmor built in but boots with SELinux as its security
# module, with no policy loaded. Two changes switch it over:
#
# 1. On Windows, %UserProfile%\.wslconfig must contain
#        [wsl2]
#        kernelCommandLine = lsm=landlock,lockdown,yama,integrity,apparmor
#    followed by `wsl --shutdown`.
# 2. Inside WSL, securityfs must be mounted, because WSL's boot doesn't mount
#    it and Ubuntu's apparmor service needs it to load profiles. This script
#    adds it to /etc/fstab.
set -euo pipefail

if [[ "$(cat /sys/module/apparmor/parameters/enabled 2>/dev/null)" != "Y" ]]; then
    echo "AppArmor is off in this kernel. Add this to %UserProfile%\\.wslconfig on Windows:" >&2
    echo "    [wsl2]" >&2
    echo "    kernelCommandLine = lsm=landlock,lockdown,yama,integrity,apparmor" >&2
    echo "then run 'wsl --shutdown' and run this script again." >&2
    exit 1
fi

if ! grep -qE '^\s*securityfs\s' /etc/fstab; then
    echo 'securityfs /sys/kernel/security securityfs defaults 0 0' | sudo tee -a /etc/fstab >/dev/null
fi
if ! mountpoint -q /sys/kernel/security; then
    sudo mount /sys/kernel/security
fi

# Load the system's profiles, then runt's.
sudo systemctl restart apparmor
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
sudo apparmor_parser --replace "$repo_dir/apparmor/runt-default"
sudo aa-status | head -n 3
echo "AppArmor is ready. Try: sudo ./runt run --rootfs /var/lib/runt/rootfs --apparmor-profile runt-default -- /bin/bash"
