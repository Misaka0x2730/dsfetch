# syntax=docker/dockerfile:1
#
# Builds dsfetch for linux/arm64. On an Apple Silicon Mac this runs natively
# (no emulation) and cgo needs no cross toolchain.
#
#   docker buildx build --platform linux/arm64 -f docker/Dockerfile \
#       --target export --output type=local,dest=build/arm64 .
#
# Base: quasimodo (Grout's gabagool image): Debian bullseye, glibc 2.31,
# SDL2 dev packages. The binary needs glibc 2.17; the console has 2.41.
# Pinned by digest; Dependabot (.github/dependabot.yml) proposes updates.
ARG BASE_IMAGE=ghcr.io/brandonkowalski/quasimodo:latest@sha256:c7201b5bc411c6cee4557940a0c12b1587ab046bb6e27bd0e497ac6ab6a6504b
FROM ${BASE_IMAGE} AS build

# quasimodo ships Go 1.25; golang.org/x/crypto and friends need >= 1.26.
ARG GO_VERSION=1.27.1
RUN ARCH="$(dpkg --print-architecture)" && \
    rm -rf /usr/local/go && \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:/root/go/bin:${PATH}" \
    GOTOOLCHAIN=local \
    CGO_ENABLED=1

WORKDIR /src
COPY go.mod go.sum ./
# gabagool is a local fork (replace directive); its go.mod lists its deps.
COPY third_party/gabagool/go.mod third_party/gabagool/go.sum ./third_party/gabagool/
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG DEBUG=0
ARG DLV_VERSION=v1.27.2
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -e; \
    BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
    LDFLAGS="-X dsfetch/internal/version.Version=${VERSION} \
             -X dsfetch/internal/version.GitCommit=${GIT_COMMIT} \
             -X dsfetch/internal/version.BuildDate=${BUILD_DATE}"; \
    mkdir -p /out; \
    if [ "$DEBUG" = "1" ]; then \
        go build -gcflags='all=-N -l' -ldflags "$LDFLAGS" -o /out/dsfetch ./cmd/dsfetch; \
        GOBIN=/out go install github.com/go-delve/delve/cmd/dlv@${DLV_VERSION}; \
    else \
        go build -trimpath -ldflags "-s -w $LDFLAGS" -o /out/dsfetch ./cmd/dsfetch; \
    fi; \
    go build -trimpath -ldflags "-s -w" -o /out/sdlprobe ./cmd/sdlprobe

# Shared libraries for firmwares that lack them; the stock one has them all,
# so a package includes them only on request:
#  lib/       - SDL2_gfx (task package BUNDLE_LIBS=1).
#  lib-extra/ - SDL2_image/SDL2_ttf and their non-glibc deps
#               (task package EXTRA_LIBS=1, which implies BUNDLE_LIBS).
COPY docker/collect-libs.sh /usr/local/bin/collect-libs
RUN set -e; \
    mkdir -p /out/lib /out/lib-extra; \
    cp -L /usr/lib/aarch64-linux-gnu/libSDL2_gfx-1.0.so.0 /out/lib/; \
    sh /usr/local/bin/collect-libs /out/lib-extra libSDL2_image-2.0.so.0 libSDL2_ttf-2.0.so.0; \
    for d in lib lib-extra; do \
        mkdir -p "/out/licenses-$d"; \
        for so in /out/$d/*; do \
            real="$(readlink -f "$(ldconfig -p | awk -v n="$(basename "$so")" '$1 == n { print $NF; exit }')")"; \
            pkg="$(dpkg -S "$real" | head -1 | cut -d: -f1)"; \
            cp "/usr/share/doc/$pkg/copyright" "/out/licenses-$d/$pkg.copyright"; \
        done; \
    done; \
    ldd --version | head -1 > /out/BUILD_GLIBC; \
    objdump -T /out/dsfetch | grep -o 'GLIBC_[0-9.]*' | sort -Vu | tail -1 > /out/REQUIRED_GLIBC

# Optional external 7z engine: the official 7zz build for arm64 from
# 7-zip.org (LGPL; needs glibc 2.17 and libstdc++). The app offers it in
# Settings when it is present and runs. Its LGPL source goes next to every
# package (dist/, and the GitHub release). A new version needs both
# checksums, the x64 one in .github/workflows/ci.yml and the version in
# THIRD_PARTY.md.
ARG SEVENZIP_VERSION=2603
ARG SEVENZIP_SHA256=2389ba20e4d8295e8709c20b6263b69bd1ec4972fe38a04ad7a1badbf595b996
ARG SEVENZIP_SRC_SHA256=9cbde5099c6deb73691b0579063da5827522ccbbcba3f0020fd04e8c8c16c0d4
RUN set -e; \
    command -v xz >/dev/null || (apt-get update && apt-get install -y --no-install-recommends xz-utils); \
    mkdir -p /tmp/7z /out/7zz; \
    cd /tmp/7z; \
    bin="7z${SEVENZIP_VERSION}-linux-arm64.tar.xz"; \
    src="7z${SEVENZIP_VERSION}-src.tar.xz"; \
    curl -fsSLO "https://www.7-zip.org/a/$bin"; \
    curl -fsSLO "https://www.7-zip.org/a/$src"; \
    printf '%s  %s\n' "$SEVENZIP_SHA256" "$bin" "$SEVENZIP_SRC_SHA256" "$src" | sha256sum -c -; \
    tar -xJf "$bin"; \
    cp 7zz /out/7zz/7zz; \
    cp License.txt /out/7zz/7-Zip-License.txt; \
    cp "$src" /out/7zz/; \
    version="${SEVENZIP_VERSION%??}.${SEVENZIP_VERSION#??}"; \
    printf '%s\n' \
        "The bundled 7zz is 7-Zip $version by Igor Pavlov: GNU LGPL with the unRAR" \
        "restriction, see 7-Zip-License.txt. Its complete source code:" \
        "https://www.7-zip.org/a/$src" \
        "The same file comes with every DSFetch release." > /out/7zz/7-Zip-SOURCE.txt; \
    /out/7zz/7zz | head -2 | tail -1 > /out/7zz/VERSION

FROM scratch AS export
COPY --from=build /out/ /
