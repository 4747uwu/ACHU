#!/usr/bin/env bash
#
# build.sh — build Achyu PACS: Windows, macOS Intel, macOS Apple Silicon.
#
#   ./build.sh                     everything this host can build
#   ./build.sh --target win        Windows installer only
#   ./build.sh --target mac        macOS only, both arches
#   ./build.sh --arch arm64        limit the macOS arches
#   ./build.sh --engine-only       just the Go binaries (runs on ANY OS)
#   ./build.sh --variant second    build "Achyu PACS 2" instead
#
# ---------------------------------------------------------------------------
# WHAT EACH HOST CAN PRODUCE
#
# The Go engine cross-compiles from anywhere — pure Go, no cgo — so any host
# produces every engine binary with --engine-only.
#
# The installers cannot. electron-builder refuses outright:
#
#     ⨯ Build for macOS is supported only on macOS
#
# Verified, not assumed. So: Windows installers on Windows, macOS on a Mac.
# The script builds whatever the host allows and says plainly what it skipped,
# rather than failing halfway through.
#
# ---------------------------------------------------------------------------
# WHY macOS TAKES TWO PASSES
#
# package.json maps ONE engine file into the bundle:
#
#     "extraResources": [{ "from": "tarang-sender", "to": "tarang-sender" }]
#
# so the engine at the repo root must match the app being packaged. This builds
# arm64, packages it, then rebuilds for x64 and packages that. `lipo` could fuse
# them instead, but then every Mac carries an engine it cannot run.

set -euo pipefail

cd "$(dirname "$0")"

# Build stderr is captured so a toolchain mismatch can be recognised and
# explained, rather than surfacing as a bare "updates to go.mod needed".
TMPERR=$(mktemp -t achyu-build.XXXXXX)
trap 'rm -f "$TMPERR"' EXIT

ARCHES="arm64 x64"
ENGINE_ONLY=0
VARIANT="primary"
TARGET="all"

while [ $# -gt 0 ]; do
  case "$1" in
    --target)      TARGET="$2"; shift 2 ;;
    --arch)        ARCHES="$2"; shift 2 ;;
    --engine-only) ENGINE_ONLY=1; shift ;;
    --variant)     VARIANT="$2"; shift 2 ;;
    -h|--help)     sed -n '2,10p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

case "$TARGET" in all|win|mac) ;; *) echo "--target must be all, win or mac" >&2; exit 2 ;; esac

HOST=$(uname -s)
case "$HOST" in
  Darwin)          CAN_WIN=0; CAN_MAC=1 ;;
  MINGW*|MSYS*|CYGWIN*) CAN_WIN=1; CAN_MAC=0 ;;
  *)               CAN_WIN=0; CAN_MAC=0 ;;
esac

say()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[33m    ! %s\033[0m\n' "$*"; }
die()  { printf '\033[31m    x %s\033[0m\n' "$*" >&2; exit 1; }

# A toolchain newer than the module's `go 1.20` directive refuses with a message
# that never mentions versions, which sends people straight to `go mod tidy` --
# and that rewrites go.mod/go.sum for EVERY build, including the Windows one
# deliberately pinned to go1.20.14 because Go dropped Windows 7 at 1.21.
# `ls -lh | awk '{print $9}'` mangles any path containing a space, and every
# artifact this produces is called "Achyu PACS ...".
size_of() {
  for f in "$@"; do
    [ -f "$f" ] || continue
    printf '    %s  %s\n' "$f" "$(du -h "$f" | cut -f1)"
  done
}

explain_toolchain() {
  grep -q "updates to go.mod needed" "$TMPERR" 2>/dev/null || return 0
  warn "That is a toolchain mismatch, not a code error."
  warn "This module targets 'go 1.20'; yours is $GO_VER."
  warn "Install Go 1.20.x:  go install golang.org/dl/go1.20.14@latest"
  warn "Do NOT run 'go mod tidy' to silence it -- that would also move the"
  warn "Windows build off go1.20.14, which Windows 7 sites still need."
}

# --- prerequisites ----------------------------------------------------------

command -v go >/dev/null || die "go not found. Install from https://go.dev/dl/"
GO_VER=$(go env GOVERSION)

# Version comes from package.json so the installer, the About screen and the
# binary can never disagree about what this build is.
APP_VER=$(node -p "require('./package.json').version" 2>/dev/null) \
  || die "could not read version from package.json (is node installed?)"

say "Achyu PACS macOS build — v$APP_VER"
echo "    toolchain : $GO_VER"
echo "    arches    : $ARCHES"
echo "    variant   : $VARIANT"

# Go 1.21 raised the floor to macOS 11. The Windows build is pinned to
# go1.20.14 because Go dropped Windows 7 at 1.21 — that constraint does not
# apply here, but the deployment target does, so say which one is in play.
case "$GO_VER" in
  go1.1*|go1.20*) echo "    min macOS : 10.13 (High Sierra)" ;;
  *)              echo "    min macOS : 11.0 (Big Sur) — Go 1.21+ dropped 10.x" ;;
esac

# --- variant ----------------------------------------------------------------

if [ -f tools/variant.js ]; then
  node tools/variant.js "$VARIANT" >/dev/null || die "variant switch failed"
fi
PRODUCT=$(node -p "require('./package.json').build.productName")
echo "    product   : $PRODUCT"

# --- engine -----------------------------------------------------------------
# Built into dist/ per arch so --engine-only leaves something useful behind,
# then copied to the root only for the arch currently being packaged.

mkdir -p dist

# --- Windows engine ---------------------------------------------------------
# 386, to match what the installer ships. Several sites still run 32-bit
# Windows, and a 64-bit engine simply will not start there.
if [ "$TARGET" = "all" ] || [ "$TARGET" = "win" ]; then
  say "engine: windows/386"
  # Straight to the REPO ROOT, not dist/, because that is where package.json
  # looks for it:  "extraResources": [{ "from": "tarang-sender.exe", ... }]
  # Building it anywhere else silently packages whatever stale binary the
  # root still holds -- an installer whose engine predates the UI it ships
  # with, which is exactly what build.bat's header warns about.
  if ! GOOS=windows GOARCH=386 CGO_ENABLED=0 \
      go build -ldflags "-X main.version=$APP_VER -s -w" \
        -o "tarang-sender.exe" ./cmd/tarang-sender/ 2>"$TMPERR"; then
    cat "$TMPERR" >&2
    explain_toolchain
    die "go build failed for windows/386"
  fi
  size_of tarang-sender.exe
fi

# --- macOS engines ----------------------------------------------------------
if [ "$TARGET" = "win" ]; then
  ARCHES=""
fi

for arch in $ARCHES; do
  case "$arch" in
    arm64) goarch=arm64 ;;
    x64)   goarch=amd64 ;;
    *) die "unknown arch: $arch (expected arm64 or x64)" ;;
  esac

  say "engine: darwin/$goarch"
  if ! GOOS=darwin GOARCH=$goarch CGO_ENABLED=0 \
      go build -ldflags "-X main.version=$APP_VER -s -w" \
        -o "dist/tarang-sender-darwin-$goarch" ./cmd/tarang-sender/ 2>"$TMPERR"; then
    cat "$TMPERR" >&2
    explain_toolchain
    die "go build failed for darwin/$goarch"
  fi

  size_of "dist/tarang-sender-darwin-$goarch"
done

if [ "$ENGINE_ONLY" = "1" ]; then
  say "engine-only build complete"
  echo "    Binaries are in dist/. Packaging the .app requires macOS."
  exit 0
fi

# --- installers -------------------------------------------------------------

command -v npx >/dev/null || die "npx not found. Install Node.js."
[ -d node_modules ] || { say "installing node dependencies"; npm install; }

BUILT=""
SKIPPED=""

# Windows
if [ "$TARGET" = "all" ] || [ "$TARGET" = "win" ]; then
  if [ "$CAN_WIN" = "1" ]; then
    say "packaging $PRODUCT for Windows"
    npx electron-builder --win --ia32 || die "electron-builder failed for Windows"
    BUILT="$BUILT windows"
  else
    SKIPPED="$SKIPPED windows(needs-Windows-host)"
  fi
fi

# macOS, one pass per architecture
if [ "$TARGET" = "all" ] || [ "$TARGET" = "mac" ]; then
  if [ "$CAN_MAC" = "1" ]; then
    for arch in $ARCHES; do
      case "$arch" in arm64) goarch=arm64 ;; x64) goarch=amd64 ;; esac
      say "packaging $PRODUCT for macOS/$arch"
      # The bundle takes whatever sits at ./tarang-sender, so put the matching
      # architecture there for this pass.
      cp "dist/tarang-sender-darwin-$goarch" tarang-sender
      chmod +x tarang-sender
      npx electron-builder --mac --"$arch" || die "electron-builder failed for macOS/$arch"
      BUILT="$BUILT macos/$arch"
    done
    rm -f tarang-sender
  else
    SKIPPED="$SKIPPED macos(needs-a-Mac)"
  fi
fi

say "done"
[ -n "$BUILT" ]   && echo "    built  :$BUILT"
[ -n "$SKIPPED" ] && warn "skipped:$SKIPPED — run this script on that host to finish it"
for f in dist/*.exe dist/*.dmg dist/*.zip; do size_of "$f"; done

if [ "$CAN_MAC" = "1" ]; then
cat <<'NOTES'

    NOTE — the macOS build is unsigned
      package.json sets identity:null, hardenedRuntime:false. Gatekeeper will
      refuse these on any Mac but the one that built them: users get "app is
      damaged and can't be opened". To distribute, sign and notarise with a
      Developer ID, or have users right-click -> Open once, or run
        xattr -dr com.apple.quarantine "/Applications/<Product>.app"
NOTES
fi
