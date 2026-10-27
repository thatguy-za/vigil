#!/bin/sh
# Runs Vigil as PUID:PGID, with access to the Pi's GPU video devices.
set -e
if [ "$(id -u)" != "0" ]; then exec vigil "$@"; fi

mkdir -p "$VIGIL_CONFIG" "$VIGIL_RECORDINGS"
chown -R "$PUID:$PGID" "$VIGIL_CONFIG"
chown "$PUID:$PGID" "$VIGIL_RECORDINGS" 2>/dev/null || true

# Add the owning group of each GPU codec node (/dev/video10-12) so a
# non-root user can use h264_v4l2m2m.
groups=""
for d in /dev/video1[0-2]; do
  [ -e "$d" ] && groups="$groups,$(stat -c %g "$d")"
done
groups="${groups#,}"

if [ -n "$groups" ]; then
  exec setpriv --reuid="$PUID" --regid="$PGID" --groups="$groups" vigil "$@"
fi
exec setpriv --reuid="$PUID" --regid="$PGID" --clear-groups vigil "$@"
