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

check_evidence=false
if [ "${1-}" = "--check-evidence" ]; then
  check_evidence=true
  shift
fi

version=${1-}
check_evidence_path=${2-}
check_source_commit=${3-}
check_package_path=${4-}
case "$version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) fail "version must use the form vX.Y.Z (for example, v0.1.0)" ;;
esac

plain_version=${version#v}
old_ifs=$IFS
IFS=.
set -- $plain_version
IFS=$old_ifs
[ "$#" -eq 3 ] || fail "version must use the form vX.Y.Z"
for component in "$@"; do
  case "$component" in
    ''|*[!0-9]*) fail "version must contain three numeric components" ;;
  esac
done

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if [ "$check_evidence" = true ]; then
  evidence_path=$check_evidence_path
  source_commit=$check_source_commit
  package_path=$check_package_path
  [ -n "$evidence_path" ] || fail "evidence manifest path is required"
  [ -n "$source_commit" ] || fail "source commit is required"
  [ -n "$package_path" ] || fail "candidate package path is required"
  exec python3 "$project_dir/scripts/validate-release-evidence.py" \
    "$evidence_path" --version "$version" --source-commit "$source_commit" --package "$package_path"
fi

release_channel=${RELEASE_CHANNEL:-candidate}
case "$release_channel" in
  candidate|production) ;;
  *) fail "RELEASE_CHANNEL must be candidate or production" ;;
esac

[ "$(uname -s)" = "Darwin" ] || fail "releases can only be built on macOS"
require_env APPLE_ID
require_env TEAM_ID
require_env APP_SIGN_ID
require_env PKG_SIGN_ID
require_env KEYCHAIN_PROFILE

for command_name in gh git make python3 security xcrun; do
  command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"
done

cd "$project_dir"

[ -z "$(git status --porcelain)" ] || fail "the Git worktree must be clean before creating a release"
branch=$(git symbolic-ref --quiet --short HEAD) || fail "releases cannot be created from a detached HEAD"
git remote get-url origin >/dev/null 2>&1 || fail "the origin Git remote is not configured"
gh auth status >/dev/null || fail "GitHub CLI authentication is required; run gh auth login"

evidence_path=""
if [ "$release_channel" = production ]; then
  evidence_path=${RELEASE_EVIDENCE-}
  [ -n "$evidence_path" ] || fail "RELEASE_EVIDENCE is required when RELEASE_CHANNEL=production"
  case "$evidence_path" in
    /*) ;;
    *) evidence_path="$project_dir/$evidence_path" ;;
  esac
  [ -f "$evidence_path" ] || fail "release evidence manifest does not exist at $evidence_path"
fi

for identity_name in "$APP_SIGN_ID" "$PKG_SIGN_ID"; do
  security find-identity -v | grep -F "$identity_name" >/dev/null || fail "a configured signing identity is not available in Keychain"
done
xcrun notarytool history --keychain-profile "$KEYCHAIN_PROFILE" >/dev/null || fail "KEYCHAIN_PROFILE is missing or invalid; run make notary-profile"

printf 'Running release checks\n'
make check

git fetch origin --tags
release_state=$(gh release view "$version" --json isDraft --jq '.isDraft' 2>/dev/null || true)
case "$release_state" in
  false)
    release_prerelease=$(gh release view "$version" --json isPrerelease --jq '.isPrerelease')
    if [ "$release_channel" = production ] && [ "$release_prerelease" = true ]; then
      tag_commit=$(git rev-parse "$version^{commit}")
      head_commit=$(git rev-parse HEAD)
      [ "$tag_commit" = "$head_commit" ] || fail "candidate tag $version does not point to the current commit"
      promotion_dir=$(mktemp -d "${TMPDIR:-/tmp}/octopus-promotion.XXXXXX")
      trap 'rm -rf "$promotion_dir"' EXIT HUP INT TERM
      promotion_package="$promotion_dir/Octopus-$plain_version.pkg"
      promotion_bundle="$promotion_dir/Octopus-$plain_version.evidence.zip"
      gh release download "$version" --pattern "Octopus-$plain_version.pkg" --dir "$promotion_dir"
      [ -f "$promotion_package" ] || fail "candidate release does not contain Octopus-$plain_version.pkg"
      python3 "$project_dir/scripts/validate-release-evidence.py" \
        "$evidence_path" --version "$version" --source-commit "$head_commit" --package "$promotion_package" \
        --bundle-output "$promotion_bundle"
      printf 'Promoting reviewed candidate %s to production\n' "$version"
      gh release upload "$version" "$evidence_path#Octopus $version reviewed production evidence" --clobber
      gh release upload "$version" "$promotion_bundle#Octopus $version verified production evidence bundle" --clobber
      gh release edit "$version" --prerelease=false --latest
      release_url=$(gh release view "$version" --json url --jq '.url')
      printf 'Promoted Octopus %s to production: %s\n' "$version" "$release_url"
      rm -rf "$promotion_dir"
      trap - EXIT HUP INT TERM
      exit 0
    fi
    fail "GitHub release $version already exists and is published"
    ;;
  true) resuming=true ;;
  '') resuming=false ;;
  *) fail "could not determine the state of GitHub release $version" ;;
esac

info_plist="$project_dir/packaging/Info.plist"
current_version=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$info_plist")

if [ "$resuming" = false ]; then
  git rev-parse -q --verify "refs/tags/$version" >/dev/null 2>&1 && fail "Git tag $version already exists without a GitHub release"

  if [ "$current_version" != "$plain_version" ]; then
    current_build=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleVersion' "$info_plist")
    case "$current_build" in
      ''|*[!0-9]*) fail "CFBundleVersion must be numeric" ;;
    esac
    next_build=$((current_build + 1))
    /usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $plain_version" "$info_plist"
    /usr/libexec/PlistBuddy -c "Set :CFBundleVersion $next_build" "$info_plist"
    git add packaging/Info.plist
    git commit -m "chore: release $version"
  fi

  printf 'Pushing release commit and tag\n'
  git push origin "HEAD:$branch"
  git tag -a "$version" -m "Octopus $version"
  git push origin "$version"

  notes_dir=$(mktemp -d "${TMPDIR:-/tmp}/octopus-release.XXXXXX")
  notes_file="$notes_dir/notes.md"
  previous_tag=$(git describe --tags --abbrev=0 "$version^" 2>/dev/null || true)
  if [ -n "$previous_tag" ]; then
    change_range="$previous_tag..$version"
  else
    change_range="$version"
  fi
  changes=$(git log "$change_range" --no-merges --format='- %s' | sed '/^- chore: release v[0-9]/d' | sed -n '1,8p')

  {
    printf 'Signed and notarized Octopus %s installer for macOS 13 or later.\n' "$version"
    if [ -n "$changes" ]; then
      printf '\n## Changes\n\n%s\n' "$changes"
    fi
    printf '\n## Install\n\nDownload the `.pkg`, open it, and follow the macOS installer.\n'
    if [ -n "$previous_tag" ]; then
      repository_url=$(gh repo view --json url --jq '.url')
      printf '\n[Full changelog](%s/compare/%s...%s)\n' "$repository_url" "$previous_tag" "$version"
    fi
  } >"$notes_file"

  printf 'Creating draft GitHub release %s\n' "$version"
  gh release create "$version" --verify-tag --draft --title "Octopus $version" --notes-file "$notes_file"
  rm -rf "$notes_dir"
else
  [ "$current_version" = "$plain_version" ] || fail "draft release exists, but packaging/Info.plist is version $current_version"
  tag_commit=$(git rev-parse "$version^{commit}")
  head_commit=$(git rev-parse HEAD)
  [ "$tag_commit" = "$head_commit" ] || fail "draft release tag $version does not point to the current commit"
  printf 'Resuming draft GitHub release %s\n' "$version"
fi

printf 'Building signed and notarized installer\n'
make installer
package_path="$project_dir/dist/Octopus-$plain_version.pkg"
[ -f "$package_path" ] || fail "installer was not created at $package_path"
checksum_path="$package_path.sha256"
modules_path="$project_dir/dist/Octopus-$plain_version.modules.json"
sbom_path="$project_dir/dist/Octopus-$plain_version.cdx.json"
[ -f "$checksum_path" ] || fail "installer checksum was not created at $checksum_path"
[ -f "$modules_path" ] || fail "dependency manifest was not created at $modules_path"
[ -f "$sbom_path" ] || fail "CycloneDX SBOM was not created at $sbom_path"
if [ "$release_channel" = production ]; then
  production_source_commit=$(git rev-parse HEAD)
  evidence_bundle="$project_dir/dist/Octopus-$plain_version.evidence.zip"
  python3 "$project_dir/scripts/validate-release-evidence.py" \
    "$evidence_path" --version "$version" --source-commit "$production_source_commit" --package "$package_path" \
    --bundle-output "$evidence_bundle"
fi

printf 'Uploading installer, checksum, module inventory, and SBOM to GitHub release\n'
gh release upload "$version" \
  "$package_path#Octopus $version macOS installer" \
  "$checksum_path#Octopus $version SHA-256 checksum" \
  "$modules_path#Octopus $version Go dependency manifest" \
  "$sbom_path#Octopus $version CycloneDX 1.6 SBOM" \
  --clobber
if [ "$release_channel" = production ]; then
  gh release upload "$version" "$evidence_path#Octopus $version reviewed production evidence" --clobber
  gh release upload "$version" "$evidence_bundle#Octopus $version verified production evidence bundle" --clobber
  gh release edit "$version" --draft=false --prerelease=false --latest
else
  gh release edit "$version" --draft=false --prerelease --latest=false
fi

release_url=$(gh release view "$version" --json url --jq '.url')
printf 'Published Octopus %s (%s): %s\n' "$version" "$release_channel" "$release_url"
