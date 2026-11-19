#!/usr/bin/env bash
# Builds and publishes a release from this Mac without GitHub Actions.
#
# The version comes from mygo.config.ts. Commit it, tag it, and push both
# before running this script:
#
#   scripts/release.sh
#
# The update key is read from MYGO_UPDATER_PRIVATE_KEY, or from the path mygo
# keygen uses when it is unset. macOS is ad-hoc signed and not notarized, so
# the first install needs Finder's Open, and later updates stay on this fork.
set -euo pipefail

cd "$(dirname "$0")/.."

version=$(sed -n 's/^  version: "\(.*\)",$/\1/p' mygo.config.ts)
if [[ -z $version ]]; then
	echo "release: no version in mygo.config.ts" >&2
	exit 1
fi

if [[ -n $(git status --porcelain) ]]; then
	echo "release: commit the release changes first" >&2
	exit 1
fi

if ! git rev-parse -q --verify "refs/tags/v$version" >/dev/null ||
	! git ls-remote --exit-code --tags origin "refs/tags/v$version" >/dev/null; then
	echo "release: push the tag v$version first" >&2
	exit 1
fi

if [[ -z ${MYGO_UPDATER_PRIVATE_KEY:-} ]]; then
	key="$HOME/Library/Application Support/mygo/update-keys/mygo-update.key"
	if [[ ! -f $key ]]; then
		echo "release: no update key at $key" >&2
		exit 1
	fi
	MYGO_UPDATER_PRIVATE_KEY="$(<"$key")"
	export MYGO_UPDATER_PRIVATE_KEY
fi

go test ./internal/...

go tool mygo build \
	-platform "darwin/arm64,darwin/amd64,windows/amd64,windows/arm64,linux/amd64,linux/arm64" \
	-sign - -skip-notarize -upload

gh release edit "v$version" --draft=false --latest

manifest="https://github.com/ybbapp/mygo-music/releases/latest/download/update-darwin-arm64.json"
if ! curl -fsSL "$manifest" | grep -q "\"version\": \"$version\""; then
	echo "release: $manifest does not name $version" >&2
	exit 1
fi

echo "Published v$version: https://github.com/ybbapp/mygo-music/releases/tag/v$version"
