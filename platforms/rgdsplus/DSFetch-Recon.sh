#!/bin/sh
# Stage 0 helper for the Ports menu: runs the device survey and sdlprobe
# without SSH, and writes the results to the root of the card this script is
# on (dsfetch-recon.txt, dsfetch-sdlprobe.txt). Read them on a PC.

PORTS_DIR="$(cd "$(dirname "$0")" && pwd)"
APP_DIR="$PORTS_DIR/DSFetch"
CARD_ROOT="$(dirname "$PORTS_DIR")"

sh "$APP_DIR/tools/device-recon.sh" > "$CARD_ROOT/dsfetch-recon.txt" 2>&1

# Try to start an SSH server if the firmware ships one but it is off.
if ! (netstat -tln 2>/dev/null || ss -tln 2>/dev/null) | grep -q ':22 '; then
    if command -v dropbear >/dev/null 2>&1; then
        mkdir -p /etc/dropbear
        dropbear -R -E >> "$CARD_ROOT/dsfetch-recon.txt" 2>&1 && echo "started dropbear" >> "$CARD_ROOT/dsfetch-recon.txt"
    elif [ -x /usr/sbin/sshd ]; then
        ssh-keygen -A >> "$CARD_ROOT/dsfetch-recon.txt" 2>&1
        /usr/sbin/sshd >> "$CARD_ROOT/dsfetch-recon.txt" 2>&1 && echo "started sshd" >> "$CARD_ROOT/dsfetch-recon.txt"
    fi
fi

export LD_LIBRARY_PATH="$APP_DIR/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
cd "$APP_DIR" && ./sdlprobe -seconds 4 -input 30 > "$CARD_ROOT/dsfetch-sdlprobe.txt" 2>&1

sync
exit 0
