#!/bin/sh
# dist.sh builds the release files into dist/: an archive per architecture
# holding a static tele and the license, the corresponding source with its dependencies vendored
# (AGPL-3.0), and SHA256SUMS over them. VERSION must be set.
set -eu

: "${VERSION:?VERSION is not set}"
: "${GO:=go}"
ARCHES=${ARCHES:-amd64 arm64}
out=dist

rm -rf "$out"
mkdir -p "$out"

for arch in $ARCHES; do
	# teleswitch is C: each architecture needs its own compiler.
	case $arch in
	amd64) cc=${CC_amd64:-x86_64-linux-gnu-gcc} ;;
	arm64) cc=${CC_arm64:-aarch64-linux-gnu-gcc} ;;
	*) echo "dist.sh: no C compiler known for $arch" >&2; exit 1 ;;
	esac
	# The whole lib directory is embedded: leave only this architecture's.
	rm -f internal/teleswitch/lib/*.so
	# tele dispatches on its own name, so it ships as "tele" in an archive
	# rather than as a renamed file.
	pkg=$out/tele-$VERSION-linux-$arch
	make --no-print-directory build GOARCH="$arch" CC="$cc" VERSION="$VERSION" BIN="$pkg/tele"
	cp LICENSE "$pkg/"
	# The AppArmor profile tele doctor installs, for packagers
	# (docs/cli.md "预检与修复策略").
	cp internal/doctor/tele.apparmor "$pkg/"
	tar -C "$out" --sort=name --owner=0 --group=0 --numeric-owner -czf "$pkg.tar.gz" "${pkg#"$out"/}"
	rm -r "$pkg"
done
rm -f internal/teleswitch/lib/*.so

# The corresponding source: the tree at HEAD plus the modules it builds
# with, so that it builds without the network.
src=tele-$VERSION
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git archive --format=tar --prefix="$src/" HEAD | tar -x -C "$tmp"
(cd "$tmp/$src" && "$GO" mod vendor)
tar -C "$tmp" --sort=name --owner=0 --group=0 --numeric-owner -czf "$out/$src-src.tar.gz" "$src"

(cd "$out" && sha256sum -- * > SHA256SUMS)
