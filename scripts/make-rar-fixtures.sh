#!/bin/sh
# Regenerates the RAR fixtures in testdata/archives. Only RARLAB's rar can
# create RAR archives; it runs in a throwaway linux/amd64 container, so
# nothing is installed on the host. Needs Docker and internet access.
# RAR 7 no longer writes the RAR 4 format, so those come from RAR 6.
set -eu

RAR_VERSION="${RAR_VERSION:-723}"
RAR4_VERSION="${RAR4_VERSION:-624}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/testdata/archives"
command -v docker >/dev/null || { echo "docker not found"; exit 1; }

mkdir -p "$OUT"
rm -f "$OUT"/*.rar "$OUT"/*.r[0-9][0-9]

docker run --rm --platform linux/amd64 -e LANG=C.UTF-8 \
    -e RAR_VERSION="$RAR_VERSION" -e RAR4_VERSION="$RAR4_VERSION" \
    -v "$OUT:/out" debian:bookworm-slim sh -eu -c '
apt-get update -qq >/dev/null && apt-get install -y -qq curl ca-certificates >/dev/null
fetch() {
    mkdir -p "/opt/$1"
    curl -fsSL "https://www.rarlab.com/rar/rarlinux-x64-$2.tar.gz" | tar -xz -C "/opt/$1" --strip-components=1
}
fetch rar "$RAR_VERSION"
fetch rar4 "$RAR4_VERSION"
rar() { cmd=$1; shift; /opt/rar/rar "$cmd" -idq -y "$@"; }
rar4() { cmd=$1; shift; /opt/rar4/rar "$cmd" -idq -y -ma4 "$@"; }

# The same content as the 7z fixtures (scripts/make-fixtures.sh).
src=/work/src
mkdir -p "$src/sub"
awk "BEGIN { for (i = 0; i < 4096; i++) printf \"ROM%05d\", i }" > "$src/game.nds"
printf "Hello from DSFetch\n" > "$src/readme.txt"
printf "nested\n" > "$src/sub/nested.txt"
cd "$src"

rar a -r -ma5          /out/rar5.rar              game.nds readme.txt sub
rar4 a -r              /out/rar4.rar              game.nds readme.txt sub
rar a -r -ma5 -s       /out/solid.rar             game.nds readme.txt sub
rar a -ma5 -pSecret123 /out/encrypted.rar         game.nds readme.txt
rar4 a -pSecret123     /out/encrypted-rar4.rar    game.nds readme.txt
rar a -ma5 -hpSecret123 /out/encrypted-headers.rar game.nds readme.txt

# Multi-volume, both naming schemes: incompressible data in 10 KB volumes.
head -c 30000 /dev/urandom > /work/noise.bin
cd /work
rar a -ma5 -m0 -v10k      /out/multivolume.rar     noise.bin
rar4 a -m0 -v10k -vn      /out/multivolume-old.rar noise.bin

# A 64 MB dictionary kept by 70 MB of zeros, for the RAM check.
head -c 70000000 /dev/zero > /work/zeros.bin
rar a -ma5 -md64m /out/bigdict.rar zeros.bin
rm /work/zeros.bin

# Cyrillic names (RAR 5 stores UTF-8).
mkdir -p "/work/cyr/Игры"
printf "тест" > "/work/cyr/Игры/Тетрис.gb"
cd /work/cyr && rar a -r -ma5 /out/cyrillic.rar "Игры"

chown -R '"$(id -u):$(id -g)"' /out
'

ls -la "$OUT"/*.rar "$OUT"/*.r[0-9][0-9] 2>/dev/null
