#!/bin/sh
set -eu

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
app_dir="$project_dir/dist/Octopus.app"
contents_dir="$app_dir/Contents"
macos_dir="$contents_dir/MacOS"
resources_dir="$contents_dir/Resources"

mkdir -p "$project_dir/dist"
rm -rf "$app_dir"
mkdir -p "$macos_dir" "$resources_dir"

app_version=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$project_dir/packaging/Info.plist")
GOWORK=off go build -trimpath -ldflags="-s -w -X github.com/sausheong/octopus/internal/buildinfo.Version=$app_version" -o "$macos_dir/octopus" "$project_dir/cmd/octopus"
cp "$project_dir/packaging/Info.plist" "$contents_dir/Info.plist"
cp "$project_dir/octopus.png" "$resources_dir/Octopus.png"
sips -s format icns "$project_dir/octopus.png" --out "$resources_dir/Octopus.icns" >/dev/null

# Seal the complete bundle so macOS validates Info.plist and the generated icon
# together with the executable. A Developer ID can replace this ad-hoc identity
# in a release pipeline without changing the bundle layout.
codesign --force --deep --sign - "$app_dir"

printf 'Built %s\n' "$app_dir"
