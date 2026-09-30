#!/bin/sh
# DSFetch entry for the stock frontend's Applications list (Roms/APPS, icon
# in Roms/APPS/Imgs/DSFetch.png). It starts the copy installed in Ports, so
# the program and its data (servers, settings) stay in one place; a copy of
# DSFetch in APPS on TF1 would be wiped by the Stock OS mod's updater, which
# empties /mnt/mmc/Roms/APPS.

card="$(cd "$(dirname "$0")/../.." 2>/dev/null && pwd)"
for ports in "$card/Ports" /mnt/sdcard/Ports /mnt/mmc/Ports; do
    if [ -f "$ports/DSFetch.sh" ] && [ -d "$ports/DSFetch" ]; then
        exec sh "$ports/DSFetch.sh"
    fi
done
echo "DSFetch is not installed in Ports on TF1 or TF2" >&2
exit 1
