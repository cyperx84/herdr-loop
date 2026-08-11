#!/bin/sh
# herdr-loop bootstrap build (PLAN.md §7).
#
# Runs on `herdr plugin install`, after user confirmation. Two paths:
#
#   1. A local Go toolchain is present: build from source. This is always the
#      preferred path — it produces an exact match for the source in this
#      checkout, not whatever the last tagged release happened to contain.
#   2. No toolchain: download a prebuilt binary from GitHub Releases and
#      verify its SHA256 before it touches disk. Verification is mandatory,
#      never optional — a checksum fetched from the same release the binary
#      came from proves nothing about tampering in transit or a compromised
#      release; it only catches truncated downloads. The checksum this script
#      trusts is pinned in EXPECTED_SHA256 below, in the source tree, not
#      fetched alongside the binary.
#
# Pattern adopted verbatim from cloudmanic/herdr-plus (220 stars, the
# highest-adoption compiled herdr plugin) per PLAN.md §7. Windows has no POSIX
# sh, so it never reaches this script — herdr-plugin.toml gives it a separate
# [[build]] entry that runs `go build` directly and requires a local Go
# toolchain.
set -eu

# cd to the plugin root regardless of where herdr invokes this from, so the
# relative "bin/herdr-loop" output path below is unambiguous.
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
cd "$root_dir"

OUT="bin/herdr-loop"
PKG="./cmd/herdr-loop"

if command -v go >/dev/null 2>&1; then
    echo "herdr-loop: building with local Go toolchain..." >&2
    exec go build -o "$OUT" "$PKG"
fi

echo "herdr-loop: no Go toolchain on PATH — falling back to a prebuilt release binary" >&2

# --- release pin -----------------------------------------------------------
#
# Bump VERSION and the platform table together, in the same commit as the
# tagged release they describe. A binary this script cannot find an entry
# for is refused, not silently skipped — see the fallback branch at the
# bottom of platform_target().
VERSION="v0.1.0"
REPO="cyperx84/herdr-loop"

os=$(uname -s)
arch=$(uname -m)

platform_target() {
    case "$os" in
        Darwin)
            case "$arch" in
                arm64) echo "darwin-arm64"; return 0 ;;
                x86_64) echo "darwin-amd64"; return 0 ;;
            esac
            ;;
        Linux)
            case "$arch" in
                x86_64) echo "linux-amd64"; return 0 ;;
                aarch64|arm64) echo "linux-arm64"; return 0 ;;
            esac
            ;;
    esac
    echo "herdr-loop: unsupported platform $os/$arch — install a Go toolchain and rerun" >&2
    return 1
}

target=$(platform_target)

# SHA256 of the release asset for each target, pinned here rather than fetched
# from the release alongside the binary (see header comment): a checksum served
# from the same place as the artifact proves only that the download was not
# truncated, not that the artifact is the one this script intended to run.
#
# Values come from the SHA256SUMS published by .github/workflows/release.yml,
# which prints them in the job log for exactly this purpose. Update both
# together when cutting a release.
expected_sha256() {
    case "$1" in
        darwin-arm64) echo "0eeb92fa1579bd82a2fcfaee34cd3906afacb5782899252c475c9455bb3822fe" ;;
        darwin-amd64) echo "deed754a0a5a388f34b8988577b6c25ed578eb2c8a8dc7e5745e02a5b54a9fe5" ;;
        linux-amd64)  echo "370728845f55da0e9959f5b0c9a675e32547b22c593bff3efa51d4ac07482ff5" ;;
        linux-arm64)  echo "177f5e992017305c06741639bf24d8dcd6a64f3d29094299f3c5346d7ff31b56" ;;
        *) return 1 ;;
    esac
}

want_sha256=$(expected_sha256 "$target") || {
    echo "herdr-loop: no pinned checksum for $target — install a Go toolchain and rerun" >&2
    exit 1
}
if [ "$want_sha256" = "UNRELEASED" ]; then
    echo "herdr-loop: no $VERSION release published yet for $target." >&2
    echo "herdr-loop: install a Go toolchain (https://go.dev/dl/) and rerun, or wait for a tagged release." >&2
    exit 1
fi

asset="herdr-loop-${VERSION}-${target}.tar.gz"
url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "herdr-loop: downloading $url" >&2
if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$tmp/$asset" "$url"
elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$tmp/$asset" "$url"
else
    echo "herdr-loop: neither curl nor wget is available — install a Go toolchain instead" >&2
    exit 1
fi

# macOS ships shasum, not sha256sum; Linux is the reverse. Try both rather
# than assuming one.
sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        echo "herdr-loop: no sha256sum/shasum available — cannot verify the download, aborting" >&2
        exit 1
    fi
}

got_sha256=$(sha256 "$tmp/$asset")
if [ "$got_sha256" != "$want_sha256" ]; then
    # Loud and fatal — never fall through to using an unverified binary.
    echo "herdr-loop: CHECKSUM MISMATCH for $asset" >&2
    echo "herdr-loop:   expected $want_sha256" >&2
    echo "herdr-loop:   got      $got_sha256" >&2
    echo "herdr-loop: refusing to install a binary that fails verification" >&2
    exit 1
fi
echo "herdr-loop: checksum verified" >&2

mkdir -p bin
tar -xzf "$tmp/$asset" -C "$tmp"
mv "$tmp/herdr-loop" "$OUT"
chmod +x "$OUT"
echo "herdr-loop: installed $OUT ($target, $VERSION)" >&2
