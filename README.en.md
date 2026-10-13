# PDF Batch Compressor (pdfcompress)

**English** | [简体中文](README.md)

An **offline batch PDF compression CLI tool** built with Go + Ghostscript. It ships as a
single binary with no installation, supports **drag-and-drop of multiple files or whole
directories**, and runs on **fully offline** intranet and office machines.

Under the hood it drives Ghostscript's `pdfwrite` device to rewrite PDF streams, reducing
size through two paths — **image downsampling** and **embedded-font subsetting** — so it
works well on both **image-heavy (scanned)** and **text-heavy (electronic document)** PDFs.

---

## Features

- **Batch processing** — pass many files or a whole directory at once; combine `-dir` with
  `-r` to recurse into subdirectories.
- **Drag-and-drop** — drop one or more PDFs (or a folder) onto the program icon to start;
  no command line needed.
- **Four compression levels** — `screen` / `ebook` / `printer` / `prepress`, to trade off
  size against clarity.
- **Parallel speed-up** — worker count defaults to the CPU core count, combined with
  single-process batching: 77 files / 593 MB measured at about 16 seconds.
- **Safe overwrite** — `-overwrite` writes a temp file first and atomically replaces on
  success; **if the result is not smaller, the original is kept**. It never inflates or
  corrupts your files.
- **Failure fallback** — whole-batch validation + per-file retry + automatic downgrade. Any
  missing output triggers reprocessing, so partial results are never reported as success.
- **Self-contained & offline** — bundled portable Ghostscript on Linux; target machines need
  nothing pre-installed and no network access.
- **Error visibility** — on Windows, when launched by double-click, the program **waits for
  Enter even on errors**, so messages never flash by unseen.

---

## Usage

### Option 1 — Drag and drop (recommended for everyday office use)

**Windows**

1. Extract the offline package to get `pdfcompress.exe` and the sibling `gs\` folder
   (**both must stay together**).
2. Select one or more PDF files, or a single folder.
3. Drag them onto `pdfcompress.exe` and release — compression starts immediately.
4. Output is written next to the source as `originalname_ebook.pdf` (the suffix is the
   active compression level).

**Linux (Kylin / UOS, ARM64 & x86_64)**

1. Extract `PDFCompress-linux-<arch>.tar.gz` (**always use `tar`, never a GUI copy**, or the
   executable bits will be lost).
2. In a file manager, drag PDFs onto the `pdfcompress` icon; or run
   `./pdfcompress file1.pdf file2.pdf`.

> Drag-and-drop is a "no arguments" launch, equivalent to batch mode below, producing
> `originalname_<level>.pdf`.

### Option 2 — Command line

```bash
# Compress a single file
pdfcompress -i input.pdf -o output.pdf

# Batch-process a directory (creates compressed/ inside the input dir by default)
pdfcompress -dir ./inbox

# Recurse, choose level and worker count
pdfcompress -dir ./inbox -r -level printer -j 8

# Overwrite originals (temp file first, atomic replace; keep original if not smaller)
pdfcompress -dir ./inbox -overwrite

# Point at a specific Ghostscript binary
pdfcompress -dir ./inbox -gs "C:\gs\bin\gswin32c.exe"
```

### Options

| Flag | Description | Default |
| --- | --- | --- |
| `-i` | Input PDF path (single-file mode) | — |
| `-o` | Output PDF path (single-file mode, mutually exclusive with `-overwrite`) | — |
| `-dir` | Input directory (batch mode) | — |
| `-out` | Output directory (batch mode) | `compressed/` inside input dir |
| `-r` | Recurse into subdirectories | `false` |
| `-level` | Compression level: `screen` / `ebook` / `printer` / `prepress` | `ebook` |
| `-j` | Number of concurrent workers | CPU cores (max 16) |
| `-gsThreads` | Render threads per Ghostscript process | `0` (auto) |
| `-persist` | Reuse a single Ghostscript process in batch mode (major speed-up) | `true` |
| `-chunk` | Files processed per Ghostscript process | `0` (auto) |
| `-overwrite` | Replace originals (kept if the result is not smaller) | `false` |
| `-gs` | Path to Ghostscript | auto-detected |
| `-q` | Quiet mode | `false` |
| `-version` | Print version information | — |

### Choosing a level

| Level | Image downsampling | Typical use |
| --- | --- | --- |
| `screen` | 72 DPI | Smallest size; **small Chinese text becomes blurry — not recommended for formal documents** |
| `ebook` | 150 DPI | **Default.** On-screen reading; best balance of size and clarity |
| `printer` | 300 DPI | For printing; roughly 3× the size of ebook |
| `prepress` | 300 DPI + mono 1200 DPI | Print-grade; largest size |

**Measured behaviour on the two common PDF types:**

- **Text-heavy (electronic documents, almost no images)** — over 99% of the size comes from
  an un-subsetted embedded CJK font stream; the main win is **font subsetting**.
  Measured 15.43 MB → about 220 KB (**-98.6%**), and **all four levels produce nearly
  byte-identical output**. This is expected, not a malfunction.
- **Image-heavy (scans)** — the four levels differ substantially. Downsampling only kicks in
  when the source image resolution exceeds the target DPI; if it is already below the target,
  images are **passed through losslessly** (which is why `printer` can be faster *and* larger).

---

## Supported systems (all fully offline)

| Platform | Architecture | Offline package | Requirements |
| --- | --- | --- | --- |
| Windows | x86 (32-bit) | `pdfcompress-windows-x86.zip` | **Windows 7 / 8 / 10 / 11**, both 32-bit and 64-bit (via WoW64) |
| Linux | ARM64 (aarch64) | `PDFCompress-linux-arm64.tar.gz` | Kylin V10 / UOS etc. **Bundled portable Ghostscript — no install, no network** |
| Linux | x86_64 (amd64) | `PDFCompress-linux-x86_64.tar.gz` | Same as above |

- **Fully offline** — every package contains everything needed at runtime. No Ghostscript
  install, no network, no Node/Python or any other runtime required.
- **Why Windows is 32-bit only** — Go 1.19 is used for Windows 7 compatibility. A single x86
  build runs on both 32-bit and 64-bit systems via WoW64, keeping deployment simple.
- **Why Linux bundles Ghostscript** — a musl statically-linked build, decoupled from the
  distro's glibc, so it is unaffected by the system Ghostscript version. The program prefers
  the `gs/` folder shipped next to it.

---

## Building from source

Requires **Go 1.19.x** (newer Go releases cannot run on Windows 7).

```bash
# Windows x86 (Win7-compatible 32-bit build)
CGO_ENABLED=0 GOOS=windows GOARCH=386 \
  go build -trimpath -buildvcs=false -ldflags "-s -w" -o pdfcompress.exe

# Linux ARM64 (Kylin / UOS)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -buildvcs=false -ldflags "-s -w" -o pdfcompress

# Linux x86_64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -ldflags "-s -w" -o pdfcompress
```

> Always build the **whole package** (`go build .` from the project root). Building a single
> file would exclude platform files such as `console_windows.go`, breaking double-click
> detection.

To shrink further, pack with UPX:

```bash
upx -9 pdfcompress.exe      # 1.81 MB → 0.72 MB
upx -9 pdfcompress          # 1.77 MB → 0.66 MB (arm64)
```

> UPX packing can trigger false positives in some antivirus products — validate whitelisting
> before deployment. Packing is lossless and reversible: `upx -d` restores a byte-identical
> file.

---

## Dependencies

- [Ghostscript](https://www.ghostscript.com/) — PDF rewriting engine (bundled in Linux packages)
- [pdfcpu](https://github.com/pdfcpu/pdfcpu) v0.15.0 — PDF structure validation

## Author

<https://github.com/playgitboy>

## License

This project is intended for internal office use and learning. Please comply with applicable
laws and software licenses. Make sure you have the right to process the target documents, and
keep backups of your originals.
