#!/bin/sh
# Builds "Keystone Node.app" (arm64 + amd64) and zips it as the release asset
# ktms-node-tray_VERSION_darwin_universal.zip. Runs on macOS: the tray needs cgo.
# Usage: scripts/build-tray-app.sh VERSION OUTDIR
set -eu
[ $# -eq 2 ] || { echo "usage: $0 VERSION OUTDIR" >&2; exit 2; }
ver=$1 out=$2
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for arch in arm64 amd64; do
	CGO_ENABLED=1 GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$ver" -o "$work/tray-$arch" ./cmd/node-tray
done
app="$work/Keystone Node.app"
mkdir -p "$app/Contents/MacOS"
lipo -create -output "$app/Contents/MacOS/ktms-node-tray" "$work/tray-arm64" "$work/tray-amd64"
# LSUIElement keeps it out of the Dock: it lives in the menu bar only.
cat >"$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key><string>ktms-node-tray</string>
	<key>CFBundleIdentifier</key><string>com.ktms.node-tray</string>
	<key>CFBundleName</key><string>Keystone Node</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>$ver</string>
	<key>CFBundleVersion</key><string>$ver</string>
	<key>LSMinimumSystemVersion</key><string>12.0</string>
	<key>LSUIElement</key><true/>
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST
plutil -lint "$app/Contents/Info.plist" >/dev/null
# Ad hoc until a Developer ID exists; Apple Silicon refuses unsigned code.
codesign --force --deep --sign - "$app"
mkdir -p "$out"
# No AppleDouble files: unzip would add them to the bundle and break its seal.
ditto -c -k --norsrc --noextattr --keepParent "$app" "$out/ktms-node-tray_${ver}_darwin_universal.zip"
echo "$out/ktms-node-tray_${ver}_darwin_universal.zip"
