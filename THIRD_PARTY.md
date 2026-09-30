# Third-party software

DSFetch is built on the following projects. The release zip carries their
full license texts in `licenses/` (collected by `scripts/collect-licenses.sh`
from the exact module versions in `go.sum`, plus the Debian copyright files
of the bundled shared libraries).

## Code adapted into DSFetch

| Project | License | Used for |
|---|---|---|
| [Grout](https://github.com/rommapp/grout) — Copyright (c) 2025 Brandon T. Kowalski, Grout Contributors | MIT (`licenses/Grout-MIT.txt`) | Archive extraction approach (`internal/archive`, adapted from Grout's `fileutil`), launcher and Docker build layout |

## Go modules linked into the binary

| Module | License |
|---|---|
| github.com/BrandonKowalski/gabagool/v2 (UI) v2.24.0, copied into `third_party/gabagool` with DSFetch changes (see `third_party/gabagool/FORK.md`) | MIT |
| github.com/BrandonKowalski/certifiable | Unlicense (public domain) |
| github.com/veandco/go-sdl2 | BSD-3-Clause |
| github.com/jlaffaye/ftp | ISC |
| github.com/pkg/sftp | BSD-2-Clause |
| github.com/kr/fs | BSD-3-Clause |
| golang.org/x/crypto, x/net, x/sys, x/text, x/image | BSD-3-Clause |
| github.com/cloudsoda/go-smb2 | BSD-2-Clause |
| **github.com/cloudsoda/sddl** (dependency of go-smb2) | **LGPL-3.0** — see note below |
| github.com/geoffgarside/ber | BSD-3-Clause |
| github.com/jcmturner/* (Kerberos support in go-smb2) | Apache-2.0 / BSD-3-Clause |
| github.com/hashicorp/go-uuid, golang-lru/v2 | MPL-2.0 |
| github.com/bodgit/sevenzip, plumbing, windows | BSD-3-Clause |
| github.com/nwaples/rardecode/v2 (RAR decoder) | BSD-2-Clause |
| github.com/ulikunitz/xz | BSD-3-Clause |
| github.com/klauspost/compress | Apache-2.0 / BSD-3-Clause |
| github.com/andybalholm/brotli | MIT |
| github.com/pierrec/lz4/v4 | BSD-3-Clause |
| github.com/stangelandcl/ppmd | MIT |
| github.com/spf13/afero | Apache-2.0 |
| go4.org | Apache-2.0 |
| github.com/nicksnyder/go-i18n/v2 | MIT |
| github.com/BurntSushi/toml | MIT |
| go.uber.org/atomic | MIT |
| github.com/holoplot/go-evdev | MIT |
| github.com/srwiley/oksvg, rasterx | BSD-3-Clause |

**LGPL-3.0: cloudsoda/sddl.** DSFetch uses the library `cloudsoda/sddl`
(through go-smb2), which is covered by the GNU Lesser General Public License
v3. Go links packages statically, so the library is part of the `dsfetch`
binary. DSFetch is open source (MIT License, see `LICENSE`), and that is how
the LGPL's relinking requirement is met: the complete source of DSFetch is
published, and `go.mod` and `go.sum` pin the exact library version (currently
`v0.0.0-20250224235906-926454e91efc`; source:
<https://github.com/cloudsoda/sddl>, also on proxy.golang.org). To run DSFetch
with a modified sddl, point a `replace` directive in `go.mod` at your copy and
rebuild with `task package`. The texts of the LGPL-3.0
(`licenses/github.com_cloudsoda_sddl.LICENSE`) and of the GPL-3.0 it builds on
(`licenses/GPL-3.0.txt`) are included.

## Fonts

gabagool embeds **HackGen Console NF** (Copyright (c) 2019 Yuko OTAWARA;
SIL Open Font License 1.1), built from Hack (Copyright (c) 2018 Source
Foundry Authors; MIT), GenJyuu Gothic (Copyright (c) 2015 JIKASEI FONT KOUBOU;
OFL 1.1) and Nerd Fonts glyphs (Copyright (c) 2014 Ryan L McIntyre; OFL 1.1).
License texts: `licenses/HackGen-OFL-1.1.txt`, `licenses/HackGen-source-*.txt`.

## Bundled binaries and libraries (release zip)

| File | Project | License |
|---|---|---|
| `7zz` | [7-Zip](https://www.7-zip.org) 26.03 by Igor Pavlov; source: [7z2603-src.tar.xz](https://www.7-zip.org/a/7z2603-src.tar.xz), also attached to each release | GNU LGPL (+ unRAR restriction); `licenses/7-Zip-License.txt`, `licenses/7-Zip-SOURCE.txt` |
| `lib/libSDL2_gfx-1.0.so.0` (only in builds made with `BUNDLE_LIBS=1`; the stock firmware has it) | SDL2_gfx | zlib; `licenses/libsdl2-gfx-1.0-0.copyright` |
| `lib/libSDL2_image*`, `lib/libSDL2_ttf*` and their dependencies (only in builds made with `EXTRA_LIBS=1`) | SDL2_image, SDL2_ttf, FreeType, libpng, libjpeg-turbo, libtiff, libwebp, zlib, zstd, xz, brotli, jbig, libdeflate | zlib / FreeType (FTL) / libpng / IJG / BSD / MIT-style; `licenses/<package>.copyright` |

## Development-only dependencies (not in the binary)

github.com/fclairamb/ftpserverlib (MIT) for the in-process FTP test server;
the Docker images in `dev/docker-compose.yml`.
