#!/usr/bin/env bash
# Build the fork's Windows desktop engine with upstream's own build/build.sh
# (BUILD_FFMPEG=yes, Win64) and package it in the official release layout,
# with a build stamp. Run it from the MSYS2 UCRT64 shell.
#
# Usage:
#   bash tools/build-desktop.sh [--tree DIR] [--out DIR] [--version STR]
#
#   --tree DIR      checkout to build (default: this repo). Must be clean and its
#                   path may hold only [A-Za-z0-9._/-] (build.sh refuses others).
#                   FFmpeg, libvpx and the delay-load libs are built into
#                   DIR/build/ (gitignored) and reused by later runs, so keep one
#                   build checkout and move it between commits.
#   --out DIR       where the zip goes (default: DIR/bin)
#   --version STR   APP_VERSION baked into the exe (default: fork-<short commit>)
#
# Go comes from PATH (GOTOOLCHAIN, GOCACHE, GOMODCACHE, GOPROXY from the caller).
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd -P)"
TREE="$here"; OUT=""; VERSION=""
while (($#)); do
	case "$1" in
		--tree) TREE="$(cd "$2" && pwd -P)"; shift 2 ;;
		--out) mkdir -p "$2"; OUT="$(cd "$2" && pwd -P)"; shift 2 ;;
		--version) VERSION="$2"; shift 2 ;;
		*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done
[[ "${MSYSTEM:-}" == "UCRT64" ]] || { echo "ERROR: run from the MSYS2 UCRT64 shell (MSYSTEM=$MSYSTEM)" >&2; exit 1; }
command -v go >/dev/null || { echo "ERROR: go not on PATH" >&2; exit 1; }
cd "$TREE"
OUT="${OUT:-$TREE/bin}"; mkdir -p "$OUT"

dirty=$(git status --porcelain | wc -l | tr -d ' ')
[[ "$dirty" == 0 ]] || { echo "ERROR: $TREE has $dirty uncommitted changes; the stamp would lie" >&2; git status --short >&2; exit 1; }
commit=$(git rev-parse HEAD)
VERSION="${VERSION:-fork-${commit:0:7}}"

# v1.0.0's build.sh hardcodes /mingw64 and would link SDL2/libxmp as plain
# imports the loader cannot find in lib/; upstream develop's uses $MINGW_PREFIX.
grep -q 'MINGW_PREFIX' build/build.sh || {
	echo "ERROR: build/build.sh predates UCRT64 (hardcodes /mingw64): build a tree with upstream develop's build.sh" >&2; exit 1; }
bs_sha=$(sha256sum build/build.sh | cut -d' ' -f1)

# The fork's rollback uses Go arenas until the upstream sync drops them.
if grep -lq '^[[:space:]]*"arena"' src/*.go; then export GOEXPERIMENT=arenas; fi
# The commit goes in the stamp, not in the exe (same rule as tools/build-engine.sh).
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false"
export CI=1   # build.sh: no "press any key" at exit

# Empty lib/ so build.sh re-bundles the DLLs (it skips when lib/ has files and
# FFmpeg was not rebuilt, which would keep stale SDL2/libxmp after a pacman update).
rm -f Ikemen_GO.exe lib/*.dll
echo "==> build/build.sh Win64 (BUILD_FFMPEG=yes, GOEXPERIMENT=${GOEXPERIMENT:-})"
BUILD_FFMPEG=yes APP_VERSION="$VERSION" bash build/build.sh Win64

# Every DLL the exe or a bundled DLL imports must be in lib/ or be a Windows
# system DLL, and the exe's NON-delayed imports must all be system DLLs: the
# loader resolves those before src/dllsearch_windows.go adds lib/ to the path.
sysdir=/c/Windows/System32
is_sys() { [[ -f "$sysdir/$1" ]] || [[ "$1" == api-ms-win-* ]] || [[ "$1" == ext-ms-* ]]; }
fail=0
static_imports() { objdump -p "$1" | awk '/^The Import Tables/{s=1} /^The Delay Import Tables|^There is a delay/{s=0} s && /DLL Name:/{print $3}'; }
# objdump does not list delay imports; build.sh makes delay libs for exactly
# these DLLs, so each one the exe names must be in lib/.
for f in build/ffmpeg/bin/*.dll "$MINGW_PREFIX"/bin/libxmp*.dll "$MINGW_PREFIX"/bin/libwinpthread-1.dll "$MINGW_PREFIX"/bin/SDL2*.dll; do
	d=$(basename "$f")
	if grep -aqF "$d" Ikemen_GO.exe && [[ ! -f "lib/$d" ]]; then echo "ERROR: Ikemen_GO.exe delay-loads $d, missing from lib/" >&2; fail=1; fi
done
for d in $(static_imports Ikemen_GO.exe); do
	is_sys "$d" || { echo "ERROR: Ikemen_GO.exe imports $d at load time (not delay-loaded)" >&2; fail=1; }
done
for f in Ikemen_GO.exe lib/*.dll; do
	for d in $(static_imports "$f"); do
		[[ -f "lib/$d" ]] || is_sys "$d" || { echo "ERROR: $f needs $d, which is neither in lib/ nor a system DLL" >&2; fail=1; }
	done
done
((fail == 0)) || exit 1

exe_sha=$(sha256sum Ikemen_GO.exe | cut -d' ' -f1)
id=${exe_sha:0:12}
name="MugenStudio-Engine-$id-windows"
stage="$OUT/$name"
rm -rf "$stage"; mkdir -p "$stage/lib"
cp Ikemen_GO.exe "$stage/"
cp lib/*.dll "$stage/lib/"
git archive HEAD data font external README.md LICENCE.txt | tar -x -C "$stage"
cp build/ffmpeg-src/BUILDINFO.txt "$stage/lib/FFmpeg-BUILDINFO.txt"

vpxc=$(git -C build/libvpx-src rev-parse HEAD)
{
	echo "# Mugen Studio desktop engine build stamp (tools/build-desktop.sh)"
	echo "engine_id = $id"
	echo "exe_sha256 = $exe_sha"
	echo "exe_bytes = $(wc -c < Ikemen_GO.exe | tr -d ' ')"
	echo "git_commit = $commit"
	echo "git_dirty_files = $dirty"
	echo "app_version = $VERSION"
	echo "go_version = $(go version)"
	echo "goexperiment = ${GOEXPERIMENT:-}"
	echo "build_command = BUILD_FFMPEG=yes APP_VERSION=$VERSION GOFLAGS=$GOFLAGS bash build/build.sh Win64"
	echo "build_sh = tree sha256 $bs_sha"
	echo "msystem = $MSYSTEM"
	echo "ffmpeg = $(sed -n 's/^Revision: //p' build/ffmpeg-src/BUILDINFO.txt)"
	echo "libvpx = $(git -C build/libvpx-src describe --tags 2>/dev/null || echo ?) commit $vpxc"
	echo "ffmpeg_configure =$(sed -n 's/^Configured with://p' build/ffmpeg-src/BUILDINFO.txt)"
	echo "msys2_packages = $(pacman -Q mingw-w64-ucrt-x86_64-SDL2 mingw-w64-ucrt-x86_64-libxmp mingw-w64-ucrt-x86_64-gcc mingw-w64-ucrt-x86_64-winpthreads 2>/dev/null | tr '\n' ';')"
	for f in lib/*.dll; do echo "dll_sha256 $(basename "$f") = $(sha256sum "$f" | cut -d' ' -f1)"; done
} > "$stage/engine-build.txt"

zip="$OUT/$name.zip"
rm -f "$zip"
# Entries at the zip root, like the official Ikemen_GO-*-windows.zip.
( cd "$stage" && /c/Windows/System32/tar.exe -a -c -f "$(cygpath -w "$zip")" $(ls -A) )
[[ "$(head -c2 "$zip")" == "PK" ]] || { echo "ERROR: $zip is not a zip" >&2; exit 1; }
echo "==> $zip ($(wc -c < "$zip" | tr -d ' ') bytes)"
echo "    stage: $stage"
