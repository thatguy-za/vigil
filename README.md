# Vigil

A small network video recorder for Raspberry Pi. It runs in Docker on 64-bit Pi 3B+, 4 and 5, and is meant to fit in 1 GB of RAM with the OS. The web UI is a PWA laid out for phones first.

It records RTSP cameras to disk: all the time, on a weekly schedule, or when something moves.

## Install

On the Pi, as the `pi` user:

```sh
bash install.sh
```

This installs Docker and Portainer if they're missing, creates `/home/pi/AppData/config` and `/home/pi/AppData/recordings`, and starts Vigil on port 8080. Open `http://<pi-ip>:8080` and create the first account.

To put recordings on a USB drive, or move the folders:

```sh
RECORDINGS=/mnt/ssd/recordings bash install.sh
APPDATA=/somewhere/else bash install.sh
```

Already running Docker? Edit `PUID`, `PGID`, `TZ` and the two folders in `docker-compose.yml`, then `docker compose up -d`. Delete the `devices:` lines on a Pi 5.

## Adding a camera

Enter the camera's address, username and password. Vigil checks for ONVIF first, then tries the stream paths used by Hikvision, Dahua/Amcrest, Reolink, Tapo, Foscam, Axis and a few generic ones. It fills in the main stream and, if there is one, the sub-stream. If nothing matches, paste the RTSP URLs yourself.

ONVIF cameras on the same network also show up on the home page by themselves.

## How it records

- **Copy, don't transcode.** The camera's stream is written to 5-minute MP4 segments as it arrives, so the CPU does almost nothing. Transcoding is an option per camera: it uses the GPU encoder (`h264_v4l2m2m`) on a Pi 3B+ or 4, and the CPU on a Pi 5, which has no H.264 encoder.
- **Motion.** Frames from the sub-stream are compared against a running background. A clip runs from 5 s before the first movement to 10 s after the last, capped at 5 minutes. You can paint zones to ignore on a 16×9 grid.
- **Retention.** Per camera, in days (0 keeps everything). Whatever the setting, the oldest recordings are deleted if free space drops below 5%.

A rough guide, not a benchmark:

| Board | Cameras in copy mode | Transcoded streams |
|---|---|---|
| Pi 3B+ | about 4 | 1, on the GPU |
| Pi 4 | about 8 | 2, on the GPU |
| Pi 5 | about 12 | 1, on the CPU |

Use a USB SSD for recordings, not the SD card.

## Users

The first account is an admin. Admins can add more users from the menu. Viewers can watch live video and recordings, and never see camera passwords. Passwords are hashed with argon2id.

If you're locked out, reset a password on the Pi:

```sh
docker exec -it vigil /entrypoint.sh reset-password <username>
```

## Not done

- HTTPS. Vigil serves plain HTTP, so keep it on your LAN or put it behind a VPN or an HTTPS proxy.
- Live view is a snapshot every couple of seconds, not video.
- No push notifications, PTZ, or per-camera permissions.
- Auto-discovery only finds ONVIF cameras.

## Releases

Publishing a GitHub release builds the arm64 image and pushes it to `ghcr.io/thatguy-za/vigil` with the version tag and `latest`.

`tools/fakeonvif` is a fake ONVIF camera for testing discovery without hardware.

## License

GNU General Public License v3. See [LICENSE](LICENSE).
