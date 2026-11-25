#!/bin/sh
set -eu

project_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
output_dir=${1:-"$PWD"}
deb_arch=${DEB_ARCH:-$(dpkg --print-architecture)}
case "$deb_arch" in
    amd64|arm64) ;;
    *) echo "Unsupported Debian architecture: $deb_arch" >&2; exit 1 ;;
esac
version=$(cat "$project_dir/VERSION")
mkdir -p "$output_dir"
output_dir=$(CDPATH='' cd -- "$output_dir" && pwd)
stage=$(mktemp -d)
chmod 0755 "$stage"
trap 'rm -rf "$stage"' EXIT
trap 'exit 1' HUP INT TERM

docs="$stage/usr/share/doc/pve-backup-gui"
install -d -m 0755 "$stage/DEBIAN" "$stage/usr/bin" "$stage/usr/lib/systemd/system" "$docs/examples" "$docs/licenses"
(
    cd "$project_dir"
    CGO_ENABLED=0 GOOS=linux GOARCH="$deb_arch"         go build -mod=vendor -trimpath -buildvcs=false -ldflags='-s -w' -o "$stage/usr/bin/pve-backup-gui" .
)
chmod 0755 "$stage/usr/bin/pve-backup-gui"

sed 's@/usr/local/bin/pve-backup-gui@/usr/bin/pve-backup-gui@' "$project_dir/deploy/pve-backup-gui.service"     > "$stage/usr/lib/systemd/system/pve-backup-gui.service"
chmod 0644 "$stage/usr/lib/systemd/system/pve-backup-gui.service"
install -m 0755 "$project_dir/deploy/debian/postinst" "$project_dir/deploy/debian/prerm"     "$project_dir/deploy/debian/postrm" "$stage/DEBIAN/"
install -m 0644 "$project_dir/config.example.json" "$project_dir/deploy/apache.conf" "$docs/examples/"
install -m 0644 "$project_dir/README.md" "$docs/README.md"
gzip -n -9 -c "$project_dir/CHANGELOG.md" > "$docs/changelog.gz"
for dependency in crypto sys term; do
    install -m 0644 "$project_dir/vendor/golang.org/x/$dependency/LICENSE" "$docs/licenses/$dependency-LICENSE"
    install -m 0644 "$project_dir/vendor/golang.org/x/$dependency/PATENTS" "$docs/licenses/$dependency-PATENTS"
done

cat > "$stage/DEBIAN/control" <<CONTROL
Package: pve-backup-gui
Version: $version
Section: admin
Priority: optional
Architecture: $deb_arch
Maintainer: PVE Backup GUI Project <pve-backup-gui@localhost>
Installed-Size: $(du -sk "$stage" | cut -f1)
Depends: adduser, ca-certificates, init-system-helpers
Description: Web interface for sequential Proxmox VE backups
 Manage plans, schedules and backup history across Proxmox VE clusters.
 Includes a systemd unit and an example TLS reverse proxy configuration.
CONTROL

dpkg-deb --root-owner-group --build "$stage" "$output_dir/pve-backup-gui_${version}_${deb_arch}.deb"
