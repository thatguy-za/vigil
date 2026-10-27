# Vigil

A lightweight NVR for Raspberry Pi (arm64: Pi 3B+, 4, 5) that runs in Docker in well under 1 GB of RAM.

- Add cameras from your phone (RTSP URL, with a "Test connection" preview)
- ONVIF auto-discovery: cameras on your network appear on the home page; enter the camera login and Vigil fills in the main and sub-stream URLs (needs `network_mode: host`)
- Record 24x7 or on a weekly schedule, in 5-minute clock-aligned MP4 segments or on motion (with 5 s pre-roll and 10 s post-roll), with paintable ignore areas
- Records the camera stream as-is (no re-encoding) by default; optional transcoding uses the Pi GPU (`h264_v4l2m2m`) on Pi 3B+/4
- Playback timeline with recorded coverage and motion events (kept in SQLite)
- Installable mobile-first PWA

## Run

```sh
docker compose up -d
```

Open `http://<pi-ip>:8080` and use "Add to Home Screen". See [docker-compose.yml](docker-compose.yml): set `PUID`, `PGID`, the config directory and the recordings directory.

## Releases

Publishing a GitHub release (e.g. `v0.1.0`) builds and pushes `ghcr.io/thatguy-za/vigil:0.1.0` and `:latest`.

## Users

The first time you open Vigil you create the admin account. Admins can add more users from the menu: **Admins** manage everything, **Viewers** can only watch live view and recordings (they never see camera passwords). Passwords are stored as argon2id hashes, and failed logins are rate-limited.

Locked out? Reset a password from the Pi:

```sh
docker exec -it vigil /entrypoint.sh reset-password <username>
```

## Notes

Vigil serves plain HTTP. For access outside your LAN, put it behind a VPN or an HTTPS reverse proxy.
