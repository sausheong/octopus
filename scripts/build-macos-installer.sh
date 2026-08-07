#!/bin/sh
set -eu

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_env() {
  name=$1
  eval "value=\${$name-}"
  [ -n "$value" ] || fail "$name is required"
}

[ "$(uname -s)" = "Darwin" ] || fail "the macOS installer can only be built on macOS"

require_env APPLE_ID
require_env TEAM_ID
require_env APP_SIGN_ID
require_env PKG_SIGN_ID
require_env KEYCHAIN_PROFILE

for command_name in codesign cyclonedx-gomod ditto go pkgbuild pkgutil python3 shasum spctl xcrun; do
  command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"
done

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
info_plist="$project_dir/packaging/Info.plist"
app_dir="$project_dir/dist/Octopus.app"
app_version=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$info_plist")
bundle_id=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$info_plist")
package_path="$project_dir/dist/Octopus-$app_version.pkg"
staging_dir=$(mktemp -d "${TMPDIR:-/tmp}/octopus-installer.XXXXXX")

cleanup() {
  rm -rf "$staging_dir"
}
trap cleanup EXIT HUP INT TERM

# APPLE_ID identifies the account stored in KEYCHAIN_PROFILE. Run
# `make notary-profile` once to create or update that profile securely.
printf 'Building Octopus %s for notarization\n' "$app_version"
"$project_dir/scripts/build-macos-app.sh"

printf 'Signing application bundle\n'
codesign --force --deep --options runtime --timestamp --sign "$APP_SIGN_ID" "$app_dir"
codesign --verify --deep --strict --verbose=2 "$app_dir"

signed_team=$(codesign -d --verbose=4 "$app_dir" 2>&1 | sed -n 's/^TeamIdentifier=//p' | head -n 1)
[ -n "$signed_team" ] || fail "the signed app has no TeamIdentifier"
[ "$signed_team" = "$TEAM_ID" ] || fail "APP_SIGN_ID does not belong to TEAM_ID"

payload_dir="$staging_dir/payload"
mkdir -p "$payload_dir/Applications"
ditto "$app_dir" "$payload_dir/Applications/Octopus.app"
rm -f "$package_path"

printf 'Building signed installer package\n'
pkgbuild \
  --root "$payload_dir" \
  --install-location / \
  --identifier "$bundle_id.pkg" \
  --version "$app_version" \
  --sign "$PKG_SIGN_ID" \
  "$package_path"
package_signature="$staging_dir/package-signature.txt"
pkgutil --check-signature "$package_path" >"$package_signature"
cat "$package_signature"
grep -F "($TEAM_ID)" "$package_signature" >/dev/null || fail "PKG_SIGN_ID does not belong to TEAM_ID"

printf 'Submitting installer to Apple notarization service\n'
xcrun notarytool submit "$package_path" \
  --keychain-profile "$KEYCHAIN_PROFILE" \
  --wait \
  --timeout 30m

printf 'Stapling and validating notarization ticket\n'
xcrun stapler staple "$package_path"
xcrun stapler validate "$package_path"
spctl --assess --type install --verbose=2 "$package_path"

checksum_path="$package_path.sha256"
modules_path="$project_dir/dist/Octopus-$app_version.modules.json"
sbom_path="$project_dir/dist/Octopus-$app_version.cdx.json"
(
  cd "$project_dir"
  GOWORK=off go list -m -json all | python3 -c '
import json, sys
decoder = json.JSONDecoder()
source = sys.stdin.read()
items = []
while source.strip():
    source = source.lstrip()
    item, end = decoder.raw_decode(source)
    items.append(item)
    source = source[end:]
json.dump(items, sys.stdout, indent=2)
sys.stdout.write("\n")
' >"$modules_path"
)
python3 -c 'import json, sys; value = json.load(open(sys.argv[1])); assert isinstance(value, list) and value' "$modules_path"
(
  cd "$project_dir"
  GOWORK=off cyclonedx-gomod app \
    -json \
    -output-version 1.6 \
    -output "$sbom_path" \
    -main cmd/octopus \
    "$project_dir"
)
python3 -c '
import json, sys
value = json.load(open(sys.argv[1]))
assert value.get("bomFormat") == "CycloneDX"
assert value.get("specVersion") == "1.6"
assert isinstance(value.get("components", []), list)
' "$sbom_path"
(
  cd "$(dirname "$package_path")"
  shasum -a 256 "$(basename "$package_path")" >"$(basename "$checksum_path")"
)

printf 'Built signed and notarized installer: %s\n' "$package_path"
printf 'Dependency manifest: %s\n' "$modules_path"
printf 'CycloneDX SBOM: %s\n' "$sbom_path"
printf 'SHA-256 checksum: %s\n' "$checksum_path"
