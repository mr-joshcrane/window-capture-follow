# window-capture-follow

Small Linux/X11 capture tool for applications that replace their windows.

Discord's current Linux screen sharing binds to one specific window. It cannot
follow an application identity across replacement windows like the old sharing
flow could. This is a problem for ShieldBattery because every Brood War game
closes the old game window and creates a new one, forcing the user to share the
new window manually.

`window-capture-follow` tracks windows by title, X11 class, and/or owning
process. It sends the current matching window to one stable virtual video
device, so Discord can keep sharing that device while games come and go.

## Requirements

- The target must be an X11 or Xwayland window.
- `xdotool` and `ffmpeg` must be installed.
- The `v4l2loopback` kernel module must be available.
- `v4l2loopback-ctl` is required for `setup`.
- `sudo` is required only when creating the virtual camera.

Install the dependencies using your distribution's package manager. Typical
package names are `ffmpeg`, `xdotool`, and `v4l2loopback` or
`v4l2loopback-utils`. The kernel module must be built for the running kernel.

For example:

```text
Brood War -> window-capture-follow -> V4L2 loopback -> Discord
```

## Build and test

```bash
go test ./...
go build .
```

## ShieldBattery

The defaults follow ShieldBattery's Brood War window and produce 720p30 video.
When no game window exists, the app shows the bundled no-signal image instead.
To verify window following and replacement without starting `ffmpeg`:

```bash
go run . watch
```

Check dependencies and system readiness without changing anything:

```bash
go run . doctor
```

Capture also runs required preflight checks automatically before starting.

Create the virtual video device explicitly:

```bash
./window-capture-follow setup
```

This explains the change, asks for confirmation, and then invokes `sudo` to
create a free device named `OBS_INGEST`. It does not modify existing devices.
The kernel chooses the device number, and the app discovers it by name whenever
it starts.

The device created by `setup` is temporary and normally disappears after a
reboot. Configure your distribution to load `v4l2loopback` persistently if
needed.

Then capture with:

```bash
go run .
```

Select `OBS_INGEST` in Discord's screen-share Devices list.

Use `Ctrl+C` to stop. The app terminates its `ffmpeg` child before exiting.

## Other applications

Override only the matching rule that differs. Rules are combined with AND, so
clear defaults that should not apply:

```bash
go run . watch --title '(?i)^My Game$' --class ''
```

Advanced options such as `--output`, `--fps`, `--display`, and
`--video-filter` remain available through `go run . --help`. Replace or disable
the bundled fallback with `--fallback-image /path/to/image` or
`--fallback-image ''`.
