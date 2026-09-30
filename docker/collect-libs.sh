#!/bin/sh
# collect-libs DEST SONAME...
# Copies the given shared libraries and, recursively, everything they list as
# NEEDED into DEST - except glibc and libSDL2 itself, which must come from the
# console (its SDL2 is built for the device's display/GPU stack).
set -e
dest="$1"; shift

resolve() {
    ldconfig -p | awk -v n="$1" '$1 == n && /aarch64|AArch64/ { print $NF; exit }'
}

skip() {
    case "$1" in
        libc.so.*|libm.so.*|libdl.so.*|libpthread.so.*|librt.so.*|libresolv.so.*|ld-linux*|libSDL2-2.0.so.*) return 0 ;;
    esac
    return 1
}

walk() {
    soname="$1"
    skip "$soname" && return 0
    [ -e "$dest/$soname" ] && return 0
    path="$(resolve "$soname")"
    if [ -z "$path" ]; then
        echo "collect-libs: cannot resolve $soname" >&2
        return 1
    fi
    cp -L "$path" "$dest/$soname"
    for dep in $(objdump -p "$path" | awk '/NEEDED/ { print $2 }'); do
        walk "$dep"
    done
}

mkdir -p "$dest"
for lib in "$@"; do
    walk "$lib"
done
ls "$dest"
