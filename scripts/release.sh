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

version=${1-}
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

[ "$(uname -s)" = "Darwin" ] || fail "releases can only be built on macOS"
require_env APPLE_ID
require_env TEAM_ID
require_env APP_SIGN_ID
require_env PKG_SIGN_ID
require_env KEYCHAIN_PROFILE

for command_name in gh git make security xcrun; do
  command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"
done

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_dir"

[ -z "$(git status --porcelain)" ] || fail "the Git worktree must be clean before creating a release"
branch=$(git symbolic-ref --quiet --short HEAD) || fail "releases cannot be created from a detached HEAD"
git remote get-url origin >/dev/null 2>&1 || fail "the origin Git remote is not configured"
gh auth status >/dev/null || fail "GitHub CLI authentication is required; run gh auth login"

for identity_name in "$APP_SIGN_ID" "$PKG_SIGN_ID"; do
  security find-identity -v | grep -F "$identity_name" >/dev/null || fail "a configured signing identity is not available in Keychain"
done
xcrun notarytool history --keychain-profile "$KEYCHAIN_PROFILE" >/dev/null || fail "KEYCHAIN_PROFILE is missing or invalid; run make notary-profile"

printf 'Running release checks\n'
make test

git fetch origin --tags
release_state=$(gh release view "$version" --json isDraft --jq '.isDraft' 2>/dev/null || true)
case "$release_state" in
  false) fail "GitHub release $version already exists and is published" ;;
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

printf 'Uploading installer to GitHub release\n'
gh release upload "$version" "$package_path#Octopus $version macOS installer" --clobber
gh release edit "$version" --draft=false --latest

release_url=$(gh release view "$version" --json url --jq '.url')
printf 'Published Octopus %s: %s\n' "$version" "$release_url"
