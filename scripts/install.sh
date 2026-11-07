#!/bin/sh
# Install nginx-sitemap. Needs only POSIX sh + coreutils `install` (no Go, no make).
#
#   From a release tarball:   tar xzf nginx-sitemap-*.tar.gz && cd nginx-sitemap-* && sudo ./install.sh
#   From a source checkout:   make build && sudo make install
#
# Options:
#   --no-systemd   do not run systemd-sysusers / systemctl daemon-reload
#   --from-source  use the repository layout (bin/, deploy/) instead of the tarball layout
# Environment:
#   DESTDIR        staging root (implies --no-systemd), e.g. for packaging
#   PREFIX         binary prefix (default /usr/local)
#
# The script never touches the nginx configuration and never reloads nginx.
# An existing /etc/nginx-sitemap/sitemap.conf is never overwritten.
set -eu

PREFIX="${PREFIX:-/usr/local}"
DESTDIR="${DESTDIR:-}"
SYSTEMD=1
LAYOUT=tarball
for a in "$@"; do
  case "$a" in
    --no-systemd) SYSTEMD=0 ;;
    --from-source) LAYOUT=source ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) echo "unknown option: $a" >&2; exit 2 ;;
  esac
done
[ -n "$DESTDIR" ] && SYSTEMD=0

HERE="$(cd "$(dirname "$0")" && pwd)"
if [ "$LAYOUT" = source ]; then
  ROOT="$(cd "$HERE/.." && pwd)"
  BIN="$ROOT/bin/nginx-sitemap"
  UNITS="$ROOT/deploy/systemd"
  CONF_EX="$ROOT/deploy/sitemap.conf.example"
  NGINX_EX="$ROOT/deploy/nginx/nginx-sitemap.conf"
  DOCS="$ROOT"
else
  BIN="$HERE/nginx-sitemap"
  UNITS="$HERE/systemd"
  CONF_EX="$HERE/sitemap.conf.example"
  NGINX_EX="$HERE/nginx/nginx-sitemap.conf"
  DOCS="$HERE"
fi

[ -x "$BIN" ] || { echo "binary not found: $BIN" >&2; exit 1; }
if [ -z "$DESTDIR" ] && [ "$(id -u)" -ne 0 ]; then
  echo "run as root (sudo ./install.sh), or set DESTDIR for a staged install" >&2
  exit 1
fi

say() { printf '  %s\n' "$*"; }
echo "Installing nginx-sitemap ($("$BIN" -version 2>/dev/null || echo unknown)) into ${DESTDIR:-/}"

install -D -m 0755 "$BIN"                                  "$DESTDIR$PREFIX/bin/nginx-sitemap"; say "$PREFIX/bin/nginx-sitemap"
install -D -m 0644 "$UNITS/nginx-sitemap.service"          "$DESTDIR/etc/systemd/system/nginx-sitemap.service"; say "/etc/systemd/system/nginx-sitemap.service"
install -D -m 0644 "$UNITS/nginx-sitemap.timer"            "$DESTDIR/etc/systemd/system/nginx-sitemap.timer"; say "/etc/systemd/system/nginx-sitemap.timer"
install -D -m 0644 "$UNITS/nginx-sitemap.sysusers.conf"    "$DESTDIR/usr/lib/sysusers.d/nginx-sitemap.conf"; say "/usr/lib/sysusers.d/nginx-sitemap.conf"
if [ "$PREFIX" != /usr/local ]; then
  # The unit's ExecStart uses /usr/local/bin; follow a custom PREFIX.
  sed -i "s|/usr/local/bin/nginx-sitemap|$PREFIX/bin/nginx-sitemap|" "$DESTDIR/etc/systemd/system/nginx-sitemap.service"
fi

DOCDIR="$DESTDIR/usr/local/share/doc/nginx-sitemap"
install -D -m 0644 "$DOCS/README.md"                                 "$DOCDIR/README.md"
install -D -m 0644 "$CONF_EX"                                        "$DOCDIR/sitemap.conf.example"
install -D -m 0644 "$NGINX_EX"                                       "$DOCDIR/nginx-sitemap.nginx.conf.sample"
install -D -m 0644 "$UNITS/nginx-sitemap.service.d/resolve.conf.example" "$DOCDIR/resolve.conf.example"
if [ -f "$DOCS/LICENSES/nginx-go-crossplane.LICENSE" ]; then
  install -D -m 0644 "$DOCS/LICENSES/nginx-go-crossplane.LICENSE"    "$DOCDIR/LICENSES/nginx-go-crossplane.LICENSE"
fi
say "/usr/local/share/doc/nginx-sitemap/ (README, samples, licenses)"

if [ -e "$DESTDIR/etc/nginx-sitemap/sitemap.conf" ]; then
  say "/etc/nginx-sitemap/sitemap.conf exists — kept unchanged"
else
  install -D -m 0644 "$CONF_EX" "$DESTDIR/etc/nginx-sitemap/sitemap.conf"; say "/etc/nginx-sitemap/sitemap.conf (from example)"
fi

if [ "$SYSTEMD" = 1 ]; then
  if command -v systemd-sysusers >/dev/null 2>&1; then
    systemd-sysusers /usr/lib/sysusers.d/nginx-sitemap.conf && say "user nginx-sitemap ensured"
  else
    echo "  systemd-sysusers not found: create the system user 'nginx-sitemap' manually" >&2
  fi
  systemctl daemon-reload && say "systemctl daemon-reload"
fi

cat <<EOF

Next steps (nothing has been started or enabled):
  1. Edit /etc/nginx-sitemap/sitemap.conf
  2. sudo systemctl start nginx-sitemap.service && journalctl -u nginx-sitemap.service -n 20
  3. sudo systemctl enable --now nginx-sitemap.timer
  4. (optional) publish: adapt /usr/local/share/doc/nginx-sitemap/nginx-sitemap.nginx.conf.sample,
     then nginx -t and reload nginx yourself.
EOF
