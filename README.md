<p align="center">
  <img src="res/diskmap.png" width="96" alt="DiskMap logo">
</p>

<h1 align="center">DiskMap</h1>

<p align="center">
  A fast, good-looking disk usage analyzer for Windows, inspired by KDE&nbsp;Filelight.<br>
  Reads the NTFS Master File Table directly — like WizTree — so a full drive scan takes seconds.
</p>

<p align="center">
  <img src="docs/screenshot.webp" alt="DiskMap screenshot" width="900">
</p>

## Features

- **Radial map** of your drive (sunburst chart). Click a slice to zoom into a folder, click the center to go up.
- **Very fast NTFS scanning.** DiskMap reads the `$MFT` straight from the volume instead of asking Windows about every folder. Falls back to a parallel directory walk on non‑NTFS drives (FAT32, exFAT) or without admin rights.
- **Sorted contents list** with size bars, synced with the map on hover.
- **Clean up in place.** Right‑click → *Show in Explorer* or *Move to Recycle Bin* (always recoverable, never a permanent delete).
- **Single portable `.exe`**, about 6 MB, no installer and no runtime to install.
- Keyboard: `Backspace` — up, `F5` — rescan, `Esc` — close menus.

## Download

Grab `DiskMap.exe` from the [Releases](../../releases) page and run it.

- Windows will ask for **administrator rights** — they are needed to read the MFT (WizTree works the same way).
- The app isn't code‑signed, so SmartScreen may warn you: **More info → Run anyway**.
- The window is rendered by Microsoft Edge in app mode (preinstalled on Windows 10/11). Chrome is used as a fallback.

## How it works

| Part | What it does |
|---|---|
| `mft.go` | Reads the NTFS boot sector, locates `$MFT`, decodes its data runs and parses every file record (fix‑ups, `$FILE_NAME`, `$DATA`, extension records) in parallel, then builds the folder tree. |
| `scan.go` | Fallback scanner: concurrent directory walk with a goroutine pool; skips symlinks and junctions. Also trims the tree for drawing. |
| `sys_windows.go` | Win32 bits: drive list, raw volume access, Recycle Bin (`SHFileOperationW`), Explorer, app window. |
| `main.go` | Local HTTP API on `127.0.0.1` with a random port and a per‑session token. |
| `web/index.html` | The whole UI: vanilla JS, SVG sunburst, no frameworks. Embedded into the binary. |

Files smaller than 256 KB are not stored one by one — they are summed per folder (“small files”) to keep memory low on drives with millions of files.

## Build from source

Requires Go 1.24+. Works from Windows, Linux or macOS:

```bash
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o DiskMap.exe .
```

The icon and the admin manifest live in `rsrc_windows_amd64.syso` (built from `res/` with `windres`), so a plain `go build` picks them up.

On Linux you can run `go run .` and open the printed URL to work on the UI (it scans with the directory walker; Recycle Bin is Windows‑only).

## Credits

- Idea and layout inspired by [Filelight](https://apps.kde.org/filelight/) (KDE) and [WizTree](https://diskanalyzer.com/).
- Visual style based on the Raycast design system from [Refero Styles](https://styles.refero.design/); background inspired by the Dot Grid effect from [React Bits](https://reactbits.dev/).

## License

[MIT](LICENSE)
