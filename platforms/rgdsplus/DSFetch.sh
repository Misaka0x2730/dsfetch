#!/bin/sh
# DSFetch launcher for the Anbernic RG DS Plus stock firmware (Ports menu).
# Install: Ports/DSFetch.sh + Ports/DSFetch/ on TF1 or TF2.

APP_DIR="$(cd "$(dirname "$0")/DSFetch" && pwd)" || exit 1
cd "$APP_DIR" || exit 1
mkdir -p data

export LD_LIBRARY_PATH="$APP_DIR/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

# Controller database: SDL reads extra mappings from this file if present.
if [ -f "$APP_DIR/data/gamecontrollerdb.txt" ]; then
    export SDL_GAMECONTROLLERCONFIG_FILE="$APP_DIR/data/gamecontrollerdb.txt"
fi

# Stock firmware input (found in Stage 0):
# * "dierct-keys-polled" is a keyboard that emits arrows and letters which
#   gabagool's keyboard map would turn into d-pad/A/L1/R2 presses on top of
#   the real gamepad, so keyboard input is off;
# * the firmware's SDL mapping names buttons by their labels (x:b3 is the top
#   button), so no Nintendo-style A/B X/Y swap is applied.
export DISABLE_KEYBOARD_INPUT="${DISABLE_KEYBOARD_INPUT:-1}"
export FLIP_FACE_BUTTONS="${FLIP_FACE_BUTTONS:-1}"

# SDL's built-in entry for "ANBERNIC-rk3568-keys" belongs to another model:
# on the RG DS Plus it turns L1 into Y, L2 into Select, R2 into Start and
# leaves Y and Menu dead. Button numbers from the device tree
# (gpio-keys-polled): A b0, B b1, Y b2, X b3, L1 b4, R1 b5, Select b6,
# Start b7, Menu b8, L3 b9, L2 b10, R2 b11, R3 b12; d-pad is hat 0. The
# stock OS mod's /mnt/mod/ctrl/configs/gamecontrollerdb.txt agrees except
# that it swaps Select and Menu (its own scripts do not).
export SDL_GAMECONTROLLERCONFIG="${SDL_GAMECONTROLLERCONFIG:-1900b655010000000100000000010000,ANBERNIC-rk3568-keys,a:b0,b:b1,y:b2,x:b3,leftshoulder:b4,rightshoulder:b5,back:b6,start:b7,guide:b8,leftstick:b9,lefttrigger:b10,righttrigger:b11,rightstick:b12,dpup:h0.1,dpdown:h0.4,dpleft:h0.8,dpright:h0.2,leftx:a0,lefty:a1,rightx:a2,righty:a3,platform:Linux,}"

# Device-specific tweaks (DSFETCH_ARGS="-screen 1", FLIP_FACE_BUTTONS=0,
# RESCAN_CMD, ...). See launcher.env.example.
if [ -f "$APP_DIR/data/launcher.env" ]; then
    . "$APP_DIR/data/launcher.env"
fi

# DSFetch follows the system "swap screens" setting itself (-screen, then
# /sys/class/anbernic_misc/lcdswap); the firmware SDL's own swap would undo it.
unset SDL2_SWAP_LCD

chmod +x ./dsfetch 2>/dev/null

# Keep the logs small: past 256 KB the current one becomes <name>.1 (the app
# rotates dsfetch.log itself). Both are opened once per run, so doing it here
# is enough.
for log in "$APP_DIR/data/launcher.log" "$APP_DIR/data/gabagool.log"; do
    [ -f "$log" ] || continue
    size=$(wc -c < "$log")
    # shellcheck disable=SC2086 # unquoted: some wc print leading spaces
    if [ $size -gt 262144 ]; then
        mv -f "$log" "$log.1"
    fi
done

# stdout/stderr go to launcher.log (crash traces); the app log is dsfetch.log.
# shellcheck disable=SC2086
./dsfetch $DSFETCH_ARGS >> "$APP_DIR/data/launcher.log" 2>&1
status=$?
echo "dsfetch exited with status $status at $(date)" >> "$APP_DIR/data/launcher.log"

# New games were added: ask the frontend to refresh its list.
if [ -f "$APP_DIR/data/.rescan" ]; then
    rm -f "$APP_DIR/data/.rescan"
    if [ -n "$RESCAN_CMD" ]; then
        sh -c "$RESCAN_CMD" >> "$APP_DIR/data/launcher.log" 2>&1
    fi
fi

sync
exit 0
