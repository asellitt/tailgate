#!/bin/sh
# Usage: ./release.sh v0.1.0
set -e
cd "$(dirname "$0")/.."

VERSION="$1"
if [ -z "$VERSION" ]; then
  echo "usage: $0 vX.Y.Z" >&2
  exit 1
fi
case "$VERSION" in
  v[0-9]*) ;;
  *) echo "version must start with v followed by a number (e.g. v0.1.0)" >&2; exit 1 ;;
esac

if [ -n "$(git status --porcelain)" ]; then
  echo "working tree is dirty; commit or stash first" >&2
  exit 1
fi
if git rev-parse "$VERSION" >/dev/null 2>&1; then
  echo "tag $VERSION already exists" >&2
  exit 1
fi

git push
git tag "$VERSION"
git push origin "$VERSION"
echo "Pushed $VERSION. Watch the build: gh run watch  (or the repo's Actions tab)"