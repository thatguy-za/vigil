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
- **Motion.** Either Vigil compares frames from the sub-stream against a running background, or it listens for the camera's own motion events over ONVIF (which costs the Pi almost nothing), or both. A clip runs from 5 s before the first movement to 10 s after the last, capped at 5 minutes. You can paint zones to ignore on a 16×9 grid. Optionally the sub-stream is also recorded all the time in low resolution, so there is something between motion clips.
- **Schedule.** Each hour of the week can be off, always recording, or motion only.
- **Retention.** Per camera, in days (0 keeps everything), plus an optional size limit per camera. Whatever the settings, the oldest recordings are deleted if free space drops below 5%. Clips you protect are never deleted automatically.
- **Health.** If a camera goes quiet for 45 seconds Vigil restarts its recorder and logs the outage. The System page shows each camera's recent uptime, the Pi's temperature and power state, and a log of events. The Storage page shows where recordings are, how much each camera uses, and warns about SD cards.

## Watching

- **Live wall.** Every camera on one page. The first four play video, the rest refresh every couple of seconds (a browser only allows about six connections to one address). Tap a camera to open it.
- **H.265.** Cameras that send H.265 play as they are in Safari and in Chrome or Edge with hardware support. Other browsers get the stream converted to H.264 on the Pi while they watch, using the GPU encoder on a Pi 3B+ or 4 and the CPU on a Pi 5. At most two streams are converted at once; any more fall back to stills.
- **Pan, tilt and zoom.** Cameras with ONVIF PTZ show arrows, zoom and saved positions under the live picture, for admins. Hold a button to move. The camera stops when you let go, or after 4 seconds if the connection drops.
- **Events.** A list of motion events from every camera, filtered by camera, by who saw the movement (Vigil or the camera) and by how far back to look. Tap one to play it.
- **Export.** On the recordings page, pick a start and end time (up to 2 hours) and download the lot as one MP4. It is cut on keyframes, so it can start a second or two early.

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
- Live video falls back to a still picture every couple of seconds when a browser can't play the stream and the Pi can't convert it.
- Exports are limited to one at a time.
- No push notifications, two-way audio, or per-camera permissions.
- Auto-discovery only finds ONVIF cameras.

## Releases

Publishing a GitHub release builds the arm64 image and pushes it to `ghcr.io/thatguy-za/vigil` with the version tag and `latest`.

`tools/fakeonvif` is a fake ONVIF camera for testing discovery without hardware.

## License

GNU General Public License v3. See [LICENSE](LICENSE).
