

<h1 align="center">go-librespot-termux</h1>

<p align="center">
  <em>A go-librespot fork tuned for running a Spotify Connect speaker on Android, via Termux.</em>
  <br>
  Turn an old phone into an always-on Spotify Connect device on your home network.
  <br>
  This fork is for my personal Android Termux home audio server, some of the changes may cause problems on other platforms.
</p>

<p align="center">
  <img alt="GitHub branch check runs" src="https://img.shields.io/github/check-runs/devgianlu/go-librespot/master">
  <a href="https://github.com/devgianlu/go-librespot/blob/master/LICENSE"><img alt="GitHub License" src="https://img.shields.io/github/license/devgianlu/go-librespot"></a>
</p>

<p align="center">
  <a href="#features">Features</a> •
  <a href="#whats-different-in-this-fork">What's Different</a> •
  <a href="#getting-started-android--termux">Getting Started</a> •
  <a href="#configuration">Configuration</a> •
  <a href="#development">Development</a>
</p>

## Features

- 🎵 **Spotify Connect** — show up as a speaker in the Spotify app and stream to it from any device on your network (Spotify Premium required).
- 🔊 **Multiple audio backends** — ALSA, PulseAudio, WASAPI on Windows, or a raw named pipe for custom routing.
- 📊 **Loudness normalization** — Spotify-standard −14 LUFS (ITU-R BS.1770) with configurable pregain.
- 🔀 **Crossfade** — configurable overlap between consecutive tracks.
- 🎙️ **Podcast resume** — episodes pick up where you left off, and progress syncs back to your other devices.
- 🎧 **DJ X** — Spotify's AI DJ, narration included: the spoken lines are synthesized and played around each track.
- 🎚️ **Flexible volume control** — independent, synchronized with the ALSA mixer, or fully external.
- 💾 **On-disk audio cache** — skip re-downloading tracks, bounded by an LRU size limit.
- 🔐 **Multiple login flows** — Zeroconf discovery, interactive OAuth, or a Spotify access token.
- 📡 **Selectable mDNS backend** — the built-in responder or the system Avahi daemon.
- 🌐 **REST API + WebSocket events** — control and monitor playback programmatically.
- 🖥️ **MPRIS integration** — control playback over D-Bus / standard Linux media keys.
- 🪶 **Lightweight & portable** — a single Go binary, ideal for Raspberry Pi and other embedded devices.
- 😴 **Sleep timer** — pause after a duration or at the end of the current track, synced back to the Spotify app's own UI.

## What's Different in This Fork

Everything below was found and fixed while running this specifically as an Android/Termux home audio server. Most of it is general reliability work that isn't Android-specific, but a few PulseAudio fixes were driven by problems only observed on Termux's PulseAudio setup. Several fixes and features originally developed here (the sleep timer, the truncated-key/seek/data-race fixes, raw dealer payload logging) have since been contributed upstream and merged there too — they're no longer listed below since they're not a difference from upstream anymore.

### Reliability / connection fixes

- Dealer/accesspoint reconnect could hang for up to 15 minutes after any disconnect, freezing all remote control — now bounded to 30s, and no longer blocks other operations (like the ping/pong keepalive) while reconnecting.
- A slow command (e.g. loading a new track) could stall the dealer's read loop and starve pong replies, itself triggering a disconnect — request handling is now decoupled from the read loop.

### PulseAudio reliability (Android/Termux)

- Fixed a hard crash (`panic: pulseaudio: no such entity`) caused by a race condition in the PulseAudio client library, triggered by a broken upstream dependency commit — downgraded to a stable release and disabled the one feature that exercised the buggy code path.
- PulseAudio calls could hang the entire daemon forever if the server stopped responding — all blocking calls are now bounded by a configurable timeout (`audio_backend_call_timeout_ms`).
- A broken output device would silently stop producing audio while Spotify's UI still said "Playing" — timed-out/broken outputs are now discarded and automatically recreated on the next play/seek attempt.
- Root-caused the most common PulseAudio resume failures: an unset buffer target let the server default to ~2000ms, which exceeds PulseAudio's internal memory pool block size and causes an instant underrun. Now pinned to a small, fixed 100ms target.
- Added a short settle delay between flushing and restarting a PulseAudio stream — needed on real hardware where an immediate restart right after a flush would hang.
- Default audio backend changed to `pulseaudio` (was `alsa`).

### New features

- Added `optimistic_playback_replies` (opt-in, off by default): reply to play/pause/seek commands immediately instead of waiting for the audio backend to confirm them.

### Patched dependency

- **`jfreymuth/pulse` is vendored and patched** in [`third_party/pulse`](/third_party/pulse), pointed at via a `replace` directive in `go.mod`. Its playback goroutine never resets its internal "bytes still owed to PulseAudio" counter when a read ends early — including on the completely ordinary end-of-stream — so the next `Start()` stacks a fresh request on top of the stale leftover. Once that total exceeds the pre-allocated buffer, it panics and takes the whole daemon down:

  ```
  panic: runtime error: slice bounds out of range [:73416] with capacity 70560
    github.com/jfreymuth/pulse.(*PlaybackStream).run() playback.go:104
  ```

  Observed in the wild after ~28 hours of uptime, triggered by hitting a restricted/unplayable track. The patch is two `requested = 0` resets; it's reported upstream and should be dropped once fixed there.

## Getting Started (Android / Termux)

This section covers running go-librespot as a home audio server on an Android phone via Termux, using a prooted Ubuntu environment for the build/runtime and PulseAudio (routed over the TCP loopback) for audio output. For every other platform, see [upstream's README](https://github.com/devgianlu/go-librespot#getting-started) instead — nothing below is specific to this fork's fixes.

### 1. Install and update Termux

Install Termux on your Android device (using [F-Droid](https://f-droid.org/packages/com.termux/) is highly recommended to get the latest updates, not the Play Store version). Once opened, update and upgrade your package lists:

```shell
pkg update && pkg upgrade
```

### 2. Set up PulseAudio, sshd, and auto-start

Install PulseAudio and OpenSSH:

```shell
pkg install pulseaudio openssh termux-api
```

`termux-api` provides `termux-wake-lock`, used below to stop Android from suspending PulseAudio in the background — install the companion [Termux:API](https://f-droid.org/packages/com.termux.api/) app too, or the wake lock silently won't do anything.

Rather than starting things manually every time, use [`termux-autostart.sh`](/termux-autostart.sh): it acquires the wake lock, starts PulseAudio listening on `127.0.0.1:4713` (so the Ubuntu proot below can reach it — it can't reach Termux's own Unix sockets), starts sshd, and drops you into the Ubuntu proot automatically. Download it into your Termux home and wire it into every new session:

```shell
curl -o ~/termux-autostart.sh https://raw.githubusercontent.com/SEKY443/go-librespot-termux/master/termux-autostart.sh
chmod +x ~/termux-autostart.sh
echo 'source ~/termux-autostart.sh' >> ~/.bashrc
```

Open a new Termux session (or `source ~/termux-autostart.sh` in this one) to run it now.

### 3. Set up Ubuntu

To ensure all the Go and C dependencies compile correctly, set up an Ubuntu environment inside Termux using proot-distro (`termux-autostart.sh` above will drop you into this automatically on future sessions):

```shell
pkg install proot-distro
proot-distro install ubuntu
proot-distro login ubuntu
```

Inside the Ubuntu proot, install a Go toolchain and this project's build dependencies, then clone and run it:

```shell
apt update && apt install -y golang libogg-dev libvorbis-dev libflac-dev libmpg123-dev libasound2-dev pkg-config git
git clone https://github.com/SEKY443/go-librespot-termux.git
cd go-librespot-termux
go run ./cmd/daemon
```

## Configuration

Configuration in this fork works exactly like upstream — see [upstream's README](https://github.com/devgianlu/go-librespot#configuration) and [`config_schema.json`](/config_schema.json) for the full list of options. The only difference is the default `audio_backend`, changed to `pulseaudio` (see [What's Different](#whats-different-in-this-fork) above).

This is the `config.yml` (in `~/.config/go-librespot/`, inside the Ubuntu proot) this fork is actually running in production:

```yaml
device_name: "<YOUR DEVICE NAME>"
device_type: "speaker"
audio_backend: "pulseaudio"
zeroconf_enabled: false
bitrate: 320
ignore_last_volume: false
initial_volume: 50
credentials:
  type: interactive
```

## Development

Protobuf definitions are managed through [Buf](https://buf.build). To recompile, execute:

```shell
buf generate
```

or using Go:

```shell
go generate ./...
```
