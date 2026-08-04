#!/bin/sh
# Wrap the desktop shell into Forge.app.
#
# A .app is a directory with a plist in it, so this is `mkdir`, `lipo` and a
# heredoc rather than a build system. Wails ships its own (`wails3 task`, driven
# by Taskfiles); adopting it would mean a second way to build this repo alongside
# the Makefile, for a bundle whose entire content is one binary and one icon.
#
# Usage: build/bundle.sh <version> <out-dir>
set -eu

VERSION=${1:?usage: bundle.sh <version> <out-dir>}
OUT=${2:?usage: bundle.sh <version> <out-dir>}
BUNDLE_ID=${BUNDLE_ID:-ai.marb.forge}

APP="$OUT/Forge.app"
LDFLAGS="-X github.com/Marb-AI/forge/internal/version.Version=$VERSION"

# CFBundleShortVersionString is what the Finder shows and what Apple validates:
# dotted numbers, nothing else. `git describe` gives v0.11.0-3-gabc1234 on
# anything but a tag, so keep the numbers and drop the rest. A build from an
# untagged tree becomes 0.0.0, which is the honest answer — the real version is
# still stamped into the binary, where `forge version` reads it.
SHORT=$(printf '%s' "$VERSION" | sed 's/^v//' | grep -oE '^[0-9]+(\.[0-9]+){0,2}' || true)
[ -n "$SHORT" ] || SHORT=0.0.0

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

echo "  icon"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM
python3 "$(dirname "$0")/icon.py" "$WORK/Forge.iconset" >/dev/null
iconutil -c icns "$WORK/Forge.iconset" -o "$APP/Contents/Resources/Forge.icns"

# Both slices, then one binary of the two: a .app is downloaded once and run on
# whatever the machine is, and a user who gets the wrong one has no way to tell
# except that it does not open. The min-version flags are the same ones `make
# app` uses, for the same reason — see the Makefile.
for arch in arm64 amd64; do
	case $arch in
	arm64) carch=arm64 ;;
	amd64) carch=x86_64 ;;
	esac
	echo "  darwin/$arch"
	GOOS=darwin GOARCH=$arch CGO_ENABLED=1 \
		CGO_CFLAGS="-arch $carch -mmacosx-version-min=11.0" \
		CGO_LDFLAGS="-arch $carch -mmacosx-version-min=11.0" \
		go build -ldflags "$LDFLAGS" -o "$APP/Contents/MacOS/forge-$arch" ./cmd/forge-app
done
lipo -create -output "$APP/Contents/MacOS/Forge" \
	"$APP/Contents/MacOS/forge-arm64" "$APP/Contents/MacOS/forge-amd64"
rm -f "$APP/Contents/MacOS/forge-arm64" "$APP/Contents/MacOS/forge-amd64"

cat >"$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>Forge</string>
	<key>CFBundleDisplayName</key><string>Forge</string>
	<key>CFBundleIdentifier</key><string>$BUNDLE_ID</string>
	<key>CFBundleExecutable</key><string>Forge</string>
	<key>CFBundleIconFile</key><string>Forge</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>$SHORT</string>
	<key>CFBundleVersion</key><string>$SHORT</string>
	<key>LSMinimumSystemVersion</key><string>11.0</string>
	<key>NSHighResolutionCapable</key><true/>
	<!-- Forge is a window, not a menu-bar accessory: it belongs in the Dock and
	     in the app switcher like anything else you open. -->
	<key>LSUIElement</key><false/>
</dict>
</plist>
PLIST
plutil -lint "$APP/Contents/Info.plist" >/dev/null
printf 'APPL????' >"$APP/Contents/PkgInfo"

# Signed ad-hoc unless a real identity is named. Ad-hoc is not a substitute for
# Developer ID — Gatekeeper still stops a downloaded copy, and the way past it is
# right-click → Open — but an unsigned bundle is refused outright on Apple
# silicon, so this is the difference between "warns" and "cannot run at all".
#
# The timestamp is the difference between the two, not a detail: an ad-hoc
# signature has no certificate to date, so asking for one is a call to Apple's
# server that buys nothing and fails on a machine that is offline. A Developer ID
# signature without one cannot be notarised at all, and stops verifying the day
# the certificate expires rather than staying good for what it signed.
IDENTITY=${CODESIGN_IDENTITY:--}
if [ "$IDENTITY" = "-" ]; then
	STAMP=--timestamp=none
else
	STAMP=--timestamp
fi
echo "  codesign ($IDENTITY)"
codesign --force --sign "$IDENTITY" --options runtime "$STAMP" "$APP"
codesign --verify --strict "$APP"

echo "$APP"
