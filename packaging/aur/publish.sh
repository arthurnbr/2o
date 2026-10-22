#!/bin/sh
# packaging/aur/publish.sh [PKG...] — push packaging/aur/<pkg>/{PKGBUILD,.SRCINFO}
# to ssh://aur@aur.archlinux.org/<pkg>.git (default: 2o 2o-bin 2o-git).
# The first push of a package creates it on the AUR under your account.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
[ $# -gt 0 ] || set -- 2o 2o-bin 2o-git
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for p; do
  (cd "$here/$p" && makepkg --printsrcinfo > .SRCINFO)
  git clone -q "ssh://aur@aur.archlinux.org/$p.git" "$tmp/$p"
  cp "$here/$p/PKGBUILD" "$here/$p/.SRCINFO" "$tmp/$p/"
  cd "$tmp/$p"
  git add PKGBUILD .SRCINFO
  if git diff --cached --quiet; then
    echo "$p: already up to date on the AUR"
  else
    ver=$(sed -n 's/^\tpkgver = //p' .SRCINFO)-$(sed -n 's/^\tpkgrel = //p' .SRCINFO)
    git commit -q -m "$p $ver"
    git push -q origin HEAD:master
    echo "$p: published $ver — https://aur.archlinux.org/packages/$p"
  fi
  cd "$here"
done
