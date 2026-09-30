#!/bin/sh
# Stage 0 device survey for the Anbernic RG DS Plus (stock firmware).
# POSIX sh / busybox compatible. Prints a plain-text report to stdout.
#
#   over SSH:   task recon CONSOLE=<ip>      (saves docs/device-report.txt)
#   on device:  Ports/DSFetch-Recon.sh       (saves <card>/dsfetch-recon.txt;
#                                             packaged only by task package RECON=1)

section() { printf '\n==================== %s\n' "$1"; }
run() { printf '$ %s\n' "$*"; sh -c "$*" 2>&1; }
have() { command -v "$1" >/dev/null 2>&1; }

section "system"
run date
run uname -a
run 'cat /etc/os-release 2>/dev/null || cat /etc/*release 2>/dev/null'
run 'cat /proc/device-tree/model 2>/dev/null; echo; tr "\0" " " < /proc/device-tree/compatible 2>/dev/null; echo'
run 'cat /proc/cmdline'
run 'grep -m1 -i "model name\|Hardware" /proc/cpuinfo; nproc 2>/dev/null'
run uptime

section "glibc / loader"
run 'ldd --version 2>&1 | head -1'
run 'ls -la /lib/ld-linux* /lib/libc.so* /lib/libc-* /lib64/ld-linux* /usr/lib/libc.so* 2>/dev/null'
run 'for f in /lib/libc.so.6 /lib/aarch64-linux-gnu/libc.so.6 /usr/lib/libc.so.6; do [ -e "$f" ] && "$f" 2>/dev/null | head -1; done'

section "memory"
run 'free -m'
run 'head -5 /proc/meminfo'
run 'cat /proc/sys/vm/swappiness 2>/dev/null; cat /proc/swaps'

section "storage"
run mount
run 'df -h'
run 'ls -la /mnt /mnt/* 2>/dev/null | head -80'
for card in /mnt/mmc /mnt/sdcard /mnt/SDCARD /mnt/sd /mnt/extsd /userdata /roms /storage; do
    if [ -d "$card" ]; then
        run "ls -la $card | head -60"
        for d in Roms roms ROMS Ports ports PORTS; do
            [ -d "$card/$d" ] && run "ls $card/$d | head -200"
        done
    fi
done

section "processes (look for the frontend)"
run 'ps -ef 2>/dev/null || ps w'
# Environment and libraries of the biggest GUI-looking process tell us how
# the stock launcher sets up SDL (SDL_VIDEODRIVER, LD_LIBRARY_PATH, ...).
for pid in $(ls /proc | grep -E '^[0-9]+$'); do
    [ -r "/proc/$pid/maps" ] || continue
    if grep -q 'libSDL2' "/proc/$pid/maps" 2>/dev/null; then
        printf '\n--- SDL process %s: %s\n' "$pid" "$(tr '\0' ' ' < /proc/$pid/cmdline)"
        echo "environment:"; tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | sort
        echo "libraries:"; awk '{print $6}' "/proc/$pid/maps" | grep '\.so' | sort -u
    fi
done

section "SDL and graphics libraries"
run 'ls -la /usr/lib/libSDL2* /usr/lib/*/libSDL2* /lib/libSDL2* /usr/local/lib/libSDL2* 2>/dev/null'
run 'find / -xdev -name "libSDL2*.so*" 2>/dev/null | head -40'
run 'find / -xdev \( -name "libfreetype.so*" -o -name "libpng16.so*" -o -name "libjpeg.so*" -o -name "libwebp.so*" -o -name "libtiff.so*" -o -name "libz.so*" -o -name "libharfbuzz.so*" \) 2>/dev/null | head -40'
run 'ls -la /usr/lib/libmali* /usr/lib/libEGL* /usr/lib/libGLES* /usr/lib/libgbm* /usr/lib/libdrm* 2>/dev/null'
run 'find / -xdev -name "gamecontrollerdb*" 2>/dev/null | head'

section "displays (DRM / framebuffer)"
run 'ls -la /dev/dri /dev/fb* 2>/dev/null'
for c in /sys/class/drm/card*-*; do
    [ -d "$c" ] || continue
    printf '%s status=%s enabled=%s modes=%s\n' "$c" \
        "$(cat "$c/status" 2>/dev/null)" "$(cat "$c/enabled" 2>/dev/null)" \
        "$(tr '\n' ' ' < "$c/modes" 2>/dev/null)"
done
run 'for f in /sys/class/graphics/fb*; do echo "$f: $(cat $f/virtual_size 2>/dev/null) $(cat $f/name 2>/dev/null)"; done'
run 'cat /sys/kernel/debug/dri/0/summary 2>/dev/null | head -40'

section "input devices"
run 'cat /proc/bus/input/devices'
run 'ls -la /dev/input /dev/input/by-* 2>/dev/null'
run 'command -v evtest || echo "evtest missing"'

section "tools"
for t in curl wget ftpget ftpput python3 python 7z 7za 7zz unzip unrar rsync dropbear sshd scp sftp-server busybox evtest gdb strace; do
    if have "$t"; then echo "present: $t -> $(command -v $t)"; else echo "missing: $t"; fi
done
run 'busybox 2>&1 | head -1; busybox --list 2>/dev/null | tr "\n" " "'

section "network"
run 'ip addr 2>/dev/null || ifconfig'
run 'iw dev 2>/dev/null; cat /proc/net/wireless 2>/dev/null'
run 'cat /etc/resolv.conf'
run 'netstat -tlnp 2>/dev/null || ss -tlnp 2>/dev/null'

section "power / sleep"
run 'cat /sys/power/state /sys/power/mem_sleep /sys/power/autosleep /sys/power/wake_lock 2>/dev/null'
run 'ls /sys/class/backlight 2>/dev/null; cat /sys/class/backlight/*/brightness 2>/dev/null'
run 'ls /etc/init.d /etc/systemd/system 2>/dev/null'
run 'grep -rl -i "suspend\|sleep" /etc/init.d /usr/bin/*.sh /usr/local/bin 2>/dev/null | head -20'

section "filesystems supported"
run 'cat /proc/filesystems'

section "done"
