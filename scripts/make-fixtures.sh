#!/bin/sh
# Regenerates the 7z fixtures in testdata/archives (zip fixtures are built by
# the Go tests themselves). Needs 7zz: brew install sevenzip
set -eu

SEVENZ="${SEVENZ:-7zz}"
command -v "$SEVENZ" >/dev/null || { echo "7zz not found (brew install sevenzip)"; exit 1; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/testdata/archives"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$OUT"
rm -f "$OUT"/*.7z "$OUT"/*.7z.0*

# Deterministic content: a small "ROM", a text file and a nested file.
src="$WORK/src"
mkdir -p "$src/sub"
awk 'BEGIN { for (i = 0; i < 4096; i++) printf "ROM%05d", i }' > "$src/game.nds"
printf 'Hello from DSFetch\n' > "$src/readme.txt"
printf 'nested\n' > "$src/sub/nested.txt"

cd "$src"
a() { "$SEVENZ" a -bso0 -bsp0 "$@"; }

a -t7z -m0=LZMA -md=1m      "$OUT/lzma.7z"      game.nds readme.txt sub
a -t7z -m0=LZMA2 -md=4m     "$OUT/lzma2.7z"     game.nds readme.txt sub
a -t7z -m0=BCJ2 -m1=LZMA -m2=LZMA -m3=LZMA -mb0s0:1 -mb0s1:2 -mb0s2:3 \
                            "$OUT/bcj2.7z"      game.nds readme.txt sub
a -t7z -m0=PPMd -mmem=16m   "$OUT/ppmd.7z"      game.nds readme.txt sub
a -t7z -ms=on               "$OUT/solid.7z"     game.nds readme.txt sub
a -t7z -ms=off              "$OUT/nonsolid.7z"  game.nds readme.txt sub
a -t7z -pSecret123          "$OUT/encrypted.7z" game.nds readme.txt
a -t7z -pSecret123 -mhe=on  "$OUT/encrypted-headers.7z" game.nds readme.txt

# Multi-volume: incompressible data split into 10 KB volumes.
head -c 30000 /dev/urandom > "$WORK/noise.bin"
cp "$WORK/noise.bin" "$src/noise.bin"
a -t7z -mx=0 -v10k "$OUT/multivolume.7z" noise.bin
rm "$src/noise.bin"

# Big dictionary in a tiny file: 70 MB of zeros keeps -md=64m (7-Zip only
# shrinks the dictionary below the data size), for the RAM check.
head -c 70000000 /dev/zero > "$WORK/zeros.bin"
(cd "$WORK" && a -t7z -m0=LZMA2 -md=64m "$OUT/bigdict.7z" zeros.bin)
rm "$WORK/zeros.bin"

# A single top-level folder (should be flattened) plus macOS/Windows junk.
mkdir -p "$WORK/wrap/Pokemon Platinum (USA)/__MACOSX"
cp "$src/game.nds" "$WORK/wrap/Pokemon Platinum (USA)/Pokemon Platinum (USA).nds"
printf 'junk' > "$WORK/wrap/Pokemon Platinum (USA)/.DS_Store"
printf 'junk' > "$WORK/wrap/Pokemon Platinum (USA)/Thumbs.db"
printf 'junk' > "$WORK/wrap/Pokemon Platinum (USA)/__MACOSX/._Pokemon Platinum (USA).nds"
(cd "$WORK/wrap" && a -t7z "$OUT/singledir.7z" "Pokemon Platinum (USA)")

# Cyrillic names (7z always stores UTF-16).
mkdir -p "$WORK/cyr/Игры"
printf 'тест' > "$WORK/cyr/Игры/Тетрис.gb"
(cd "$WORK/cyr" && a -t7z "$OUT/cyrillic.7z" "Игры")

ls -la "$OUT"
