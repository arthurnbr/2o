#!/bin/sh
# packaging/release.sh X.Y.Z — after the GitHub release vX.Y.Z is published,
# point the AUR packages and the Homebrew cask at it, with its checksums.
set -eu
v=${1:?usage: packaging/release.sh X.Y.Z}
v=${v#v}
root=$(cd "$(dirname "$0")/.." && pwd)
base=https://github.com/arthurnbr/2o
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "$base/releases/download/v$v/SHA256SUMS" -o "$tmp/SHA256SUMS"
sum() { awk -v f="$1" '$2 == f { print $1 }' "$tmp/SHA256SUMS" | grep . || { echo "no $1 in v$v SHA256SUMS" >&2; exit 1; }; }
curl -fsSL "$base/archive/refs/tags/v$v.tar.gz" -o "$tmp/src.tar.gz"
src=$(sha256sum "$tmp/src.tar.gz" | cut -d ' ' -f 1)
curl -fsSL "https://raw.githubusercontent.com/arthurnbr/2o/v$v/LICENSE" -o "$tmp/LICENSE"
lic=$(sha256sum "$tmp/LICENSE" | cut -d ' ' -f 1)

aur=$root/packaging/aur
sed -i -e "s/^pkgver=.*/pkgver=$v/" -e "s/^pkgrel=.*/pkgrel=1/" \
  -e "s/^sha256sums=.*/sha256sums=('$src')/" "$aur/2o/PKGBUILD"
sed -i -e "s/^pkgver=.*/pkgver=$v/" -e "s/^pkgrel=.*/pkgrel=1/" \
  -e "s/^sha256sums=.*/sha256sums=('$lic')/" \
  -e "s/^sha256sums_x86_64=.*/sha256sums_x86_64=('$(sum "2o_${v}_linux_amd64.tar.gz")')/" \
  -e "s/^sha256sums_aarch64=.*/sha256sums_aarch64=('$(sum "2o_${v}_linux_arm64.tar.gz")')/" \
  "$aur/2o-bin/PKGBUILD"
for p in 2o 2o-bin 2o-git; do
  (cd "$aur/$p" && makepkg --printsrcinfo > .SRCINFO)
done

cask=$root/Casks/2o.rb
sed -i -e "s/^  version \".*\"/  version \"$v\"/" \
  -e "s/^  sha256 arm: *\"[0-9a-f]*\"/  sha256 arm:          \"$(sum "2o_${v}_darwin_arm64.tar.gz")\"/" \
  -e "s/^         intel: *\"[0-9a-f]*\"/         intel:        \"$(sum "2o_${v}_darwin_amd64.tar.gz")\"/" \
  -e "s/^         arm64_linux: *\"[0-9a-f]*\"/         arm64_linux:  \"$(sum "2o_${v}_linux_arm64.tar.gz")\"/" \
  -e "s/^         x86_64_linux: *\"[0-9a-f]*\"/         x86_64_linux: \"$(sum "2o_${v}_linux_amd64.tar.gz")\"/" \
  "$cask"
grep -q "$(sum "2o_${v}_darwin_arm64.tar.gz")" "$cask" || { echo "cask checksums not updated" >&2; exit 1; }

echo "2o $v: packaging/aur/{2o,2o-bin,2o-git} and Casks/2o.rb updated"
