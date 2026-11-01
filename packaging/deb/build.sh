#!/usr/bin/env bash
# Builds dist/persona_<version>_amd64.deb: the persona CLI plus everything the
# Firecracker runtime needs on a Debian (trixie or later) x86_64 host.
#
#   packaging/deb/build.sh                 # latest Firecracker CI guest kernel
#   KERNEL=/path/to/vmlinux packaging/deb/build.sh
#
# Downloads are cached in .cache/deb and checksum-verified where upstream
# publishes checksums.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
HERE="$ROOT/packaging/deb"
FC_VERSION=${FC_VERSION:-v1.17.0}
CACHE=${CACHE:-$ROOT/.cache/deb}
OUT=${OUT:-$ROOT/dist}
S3=https://s3.amazonaws.com/spec.ccfc.min

cd "$ROOT"
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
# Only release tags (v1.2.3, 1.2.3-4-gabc) become versions; a bare commit hash
# may start with a digit and would otherwise sort above every future release.
if [[ $VERSION =~ ^v?[0-9]+\.[0-9]+ ]]; then
	DEBVER=${VERSION#v}
else
	DEBVER="0.0.0~git$(git log -1 --format=%cd.%h --date=format:%Y%m%d 2>/dev/null || date +%Y%m%d)"
	[[ $VERSION == *-dirty ]] && DEBVER+="+dirty"
fi
DEBVER=${DEBVER//-/+} # dashes are reserved for Debian revisions

mkdir -p "$CACHE" "$OUT"
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
install -d "$STAGE"/{DEBIAN,usr/bin,usr/libexec/persona,usr/share/persona,usr/share/doc/persona/firecracker,usr/lib/systemd/system,usr/lib/modules-load.d,etc/sudoers.d}

echo "== Go binaries ($VERSION)"
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
go build -trimpath -ldflags "-s -w -X github.com/johndavisdev/persona/cmd.Version=$VERSION" -o "$STAGE/usr/bin/persona" .
go build -trimpath -ldflags "-s -w" -o "$STAGE/usr/libexec/persona/persona-vm" ./cmd/persona-vm

echo "== persona marketplace (plugin beacon, with persona-beacon, in every image)"
if [ -n "${MARKETPLACE:-}" ]; then
	cp -R "$MARKETPLACE" "$STAGE/usr/share/persona/marketplace"
else
	command -v dotnet >/dev/null || { echo "persona-beacon needs the .NET SDK (or pass MARKETPLACE=/path/to/published/marketplace)" >&2; exit 1; }
	bdir=$(mktemp -d -p "$STAGE")
	dotnet publish "$ROOT/beacon/Beacon.csproj" -nologo -v q -c Release -r linux-x64 -o "$bdir"
	cp -R "$bdir/marketplace" "$STAGE/usr/share/persona/marketplace"
	rm -rf "$bdir"
fi

# Claude Code managed settings, installed in Claude Code personas' images.
install -m 0644 "$ROOT/managed-settings.json" "$STAGE/usr/share/persona/managed-settings.json"

echo "== Firecracker $FC_VERSION"
tgz="firecracker-$FC_VERSION-x86_64.tgz"
if [ ! -s "$CACHE/$tgz" ]; then
	base="https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION"
	curl -fsSL -o "$CACHE/$tgz.sha256.txt" "$base/$tgz.sha256.txt"
	curl -fsSL -o "$CACHE/$tgz.partial" "$base/$tgz"
	mv "$CACHE/$tgz.partial" "$CACHE/$tgz"
fi
(cd "$CACHE" && sha256sum --quiet -c "$tgz.sha256.txt")
fcdir=$(mktemp -d -p "$STAGE")
tar -xzf "$CACHE/$tgz" -C "$fcdir"
rel="$fcdir/release-$FC_VERSION-x86_64"
install -m 0755 "$rel/firecracker-$FC_VERSION-x86_64" "$STAGE/usr/libexec/persona/firecracker"
install -m 0755 "$rel/jailer-$FC_VERSION-x86_64" "$STAGE/usr/libexec/persona/jailer"
for f in LICENSE NOTICE THIRD-PARTY; do install -m 0644 "$rel/$f" "$STAGE/usr/share/doc/persona/firecracker/$f"; done
rm -rf "$fcdir"

echo "== Guest kernel"
if [ -n "${KERNEL:-}" ]; then
	install -m 0644 "$KERNEL" "$STAGE/usr/share/persona/vmlinux"
	[ -f "$KERNEL.config" ] && install -m 0644 "$KERNEL.config" "$STAGE/usr/share/doc/persona/vmlinux.config"
else
	prefix=$(curl -fsSL "$S3?list-type=2&prefix=firecracker-ci/&delimiter=/" |
		grep -oP "(?<=<Prefix>)firecracker-ci/[0-9]{8}-[^/]+/(?=</Prefix>)" | sort | tail -1)
	key=$(curl -fsSL "$S3?list-type=2&prefix=${prefix}x86_64/vmlinux-" |
		grep -oP "(?<=<Key>)(${prefix}x86_64/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3})(?=</Key>)" | sort -V | tail -1)
	[ -n "$key" ] || { echo "no guest kernel found; pass KERNEL=/path/to/vmlinux" >&2; exit 1; }
	kfile="$CACHE/$(echo "$prefix" | cut -d/ -f2)-$(basename "$key")"
	if [ ! -s "$kfile" ]; then
		curl -fsSL -o "$kfile.partial" "$S3/$key" && mv "$kfile.partial" "$kfile"
		curl -fsSL -o "$kfile.config" "$S3/$key.config"
	fi
	echo "   $(basename "$key") from $prefix"
	install -m 0644 "$kfile" "$STAGE/usr/share/persona/vmlinux"
	install -m 0644 "$kfile.config" "$STAGE/usr/share/doc/persona/vmlinux.config"
fi
if [ -f "$STAGE/usr/share/doc/persona/vmlinux.config" ]; then
	for opt in VIRTIO_VSOCKETS OVERLAY_FS DEVTMPFS IP_PNP EXT4_FS; do
		grep -q "^CONFIG_$opt=y" "$STAGE/usr/share/doc/persona/vmlinux.config" ||
			{ echo "guest kernel lacks CONFIG_$opt=y" >&2; exit 1; }
	done
fi

echo "== Scripts, units, config"
install -m 0755 "$HERE/persona-netns" "$STAGE/usr/libexec/persona/persona-netns"
install -m 0644 "$HERE/persona-netns.service" "$STAGE/usr/lib/systemd/system/persona-netns.service"
install -m 0644 "$HERE/persona-dns.service" "$STAGE/usr/lib/systemd/system/persona-dns.service"
install -m 0644 "$HERE/modules-load.conf" "$STAGE/usr/lib/modules-load.d/persona.conf"
install -m 0440 "$HERE/sudoers" "$STAGE/etc/sudoers.d/persona-vm"
/usr/sbin/visudo -cqf "$STAGE/etc/sudoers.d/persona-vm"
install -m 0644 "$HERE/copyright" "$STAGE/usr/share/doc/persona/copyright"
install -m 0644 "$ROOT/README.md" "$STAGE/usr/share/doc/persona/README.md"
for s in postinst prerm postrm; do install -m 0755 "$HERE/$s" "$STAGE/DEBIAN/$s"; done
echo /etc/sudoers.d/persona-vm > "$STAGE/DEBIAN/conffiles"
size=$(du -sk --exclude=DEBIAN "$STAGE" | cut -f1)
sed -e "s/@VERSION@/$DEBVER/" -e "s/@SIZE@/$size/" -e "s/@FC_VERSION@/$FC_VERSION/" "$HERE/control.in" > "$STAGE/DEBIAN/control"
chmod -R u+rwX,go+rX,go-w "$STAGE/usr"
chmod 0440 "$STAGE/etc/sudoers.d/persona-vm"

deb="$OUT/persona_${DEBVER}_amd64.deb"
dpkg-deb --root-owner-group -Zxz --build "$STAGE" "$deb" >/dev/null
echo "== $deb ($(du -h "$deb" | cut -f1))"
