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
- **Smart scan detection** — automatically detects PDFs where image pixels were written as
  the page size (common with phone/scanner exports), corrects the page geometry basis, and
  only then downsamples — so these files actually compress (see "Scan geometry auto-fix").
- **Parallel speed-up** — worker count defaults to the CPU core count, combined with
  single-process batching: 98 files / 948 MB measured at about 3.5 minutes.
- **Safe overwrite** — `-overwrite` writes a temp file first and atomically replaces on
  success; **if the result is not smaller, the original is kept**. It never inflates or
  corrupts your files.
- **Failure fallback** — whole-batch validation + per-file retry + automatic downgrade. Any
  missing output triggers reprocessing, so partial results are never reported as success.
- **Self-contained & offline** — bundled portable Ghostscript on Linux; target machines need
  nothing pre-installed and no network access.
- **Zero third-party dependencies** — geometry probing and argument assembly are pure Go
  standard library.
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

1. Extract `PDFCompress-linux-<arch>.tar.xz` (**always use `tar`, never a GUI copy**, or the
   executable bits will be lost):
   ```bash
   tar -xf PDFCompress-linux-arm64.tar.xz
   cd PDFCompress-linux-arm64
   ```
2. **Enable drag-and-drop** (recommended, run once):
   ```bash
   sh 安装拖放.sh
   ```
   The script fixes permissions, creates a "PDF压缩" icon on the desktop, and registers the
   app in the application menu. Afterwards select PDF files (or a folder) and **drop them onto
   that icon** — a terminal window opens showing progress. If the icon shows "untrusted",
   right-click it and choose "Allow launching".

3. Or use the command line directly:
   ```bash
   ./pdfcompress file1.pdf file2.pdf    # shorthand equivalent to drag-and-drop
   ./pdfcompress -dir ./inbox -r        # recursive batch
   ```

> **Why `安装拖放.sh` is needed**: Linux desktops (GNOME/KDE/UKUI/DDE) implement
> "drop a file onto an app icon" as a call of the form `program file1 file2 ...`, which
> requires a `.desktop` launcher to declare that entry point. The program already treats
> positional arguments as files/directories to process when launched with no flags, so the
> script only needs to generate a `.desktop` file plus a terminal wrapper — **no program
> logic changes**. It auto-detects 10 common terminal emulators (ukui-terminal,
> deepin-terminal, gnome-terminal, konsole, xterm, …) and waits for Enter when done, so the
> window never flashes by.

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

# Custom DPI (e.g. 100 PPI, for a size between screen and ebook)
pdfcompress -i input.pdf -level 100

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
| `-level` | Compression level: `screen` / `ebook` / `printer` / `prepress`, or a custom PPI value (e.g. `100`) | `ebook` |
| `-j` | Number of concurrent workers | CPU cores (max 16) |
| `-gsThreads` | Render threads per Ghostscript process | `0` (auto) |
| `-persist` | Reuse a single Ghostscript process in batch mode (major speed-up) | `true` |
| `-chunk` | Files processed per Ghostscript process | `0` (auto) |
| `-overwrite` | Replace originals (kept if the result is not smaller) | `false` |
| `-gs` | Path to Ghostscript | auto-detected |
| `-timeout` | Stall guard: skip a file after this many seconds with no progress output (`0` = disabled) | `180` |
| `-q` | Quiet mode | `false` |
| `-version` | Print version information | — |

### Choosing a level

| Level | Image downsampling | Typical use |
| --- | --- | --- |
| `screen` | 72 DPI | Smallest size; **small Chinese text becomes blurry — not recommended for formal documents** |
| `ebook` | 150 DPI | **Default.** On-screen reading; best balance of size and clarity |
| `printer` | 300 DPI | For printing; roughly 3× the size of ebook |
| `prepress` | 300 DPI + mono 1200 DPI | Print-grade; largest size |
| `10~2400` | Custom value | e.g. `-level 100` for a middle ground between screen and ebook |

**Custom PPI values:** `-level` accepts an integer from 10 to 2400 (e.g. `-level 100`).  
Lower values mean smaller files but lower text clarity; for on-screen reading 72 is the minimum, 100+ is recommended for regular use.

**Measured behaviour on the two common PDF types:**

- **Text-heavy (electronic documents, almost no images)** — over 99% of the size comes from
  an un-subsetted embedded CJK font stream; the main win is **font subsetting**.
  Measured 15.43 MB → about 220 KB (**-98.6%**), and **all four levels produce nearly
  byte-identical output**. This is expected, not a malfunction.
- **Image-heavy (scans)** — the four levels differ substantially. **Any image whose effective
  resolution exceeds the target DPI is downsampled**, so a higher level keeps more detail and
  produces a larger file (`printer` is therefore normally much larger than `ebook`). Images
  already below the target are never upsampled, so they cannot inflate the output.

---

## Scan geometry auto-fix

### The problem: why do some scans refuse to compress?

Phone photos and scanner exports often write the **image pixel count directly as the page size
in points**. For example a 2736×3648 pixel photo becomes a 2736×3648 pt page (normal A4 is only
595×842 pt).

Ghostscript then computes the **effective DPI as 72** (2736 px ÷ (2736 pt ÷ 72)), which is below
every compression threshold, so it decides "no downsampling needed" — `screen` / `ebook` /
`printer` outputs become **nearly byte-identical**, and the compression ratio is ~0. This is the
real root cause of "big file but won't compress".

### The fix: smart decision, not a blunt hammer

The program includes a **zero-dependency plain-text geometry probe** (`pdfgeom.go`) that scans
`/MediaBox` values and applies a **three-branch decision**:

| Decision | Condition | Action |
| --- | --- | --- |
| **Standard paper** | Matches A4 / A3 / A5 / Letter / Legal etc. | **No change at all**; uses original logic |
| **Pixel-as-point** | Oversized (long edge > 1300 pt) **and** contains images **and** abnormal pages are the majority | Enable geometry correction |
| **Undetermined** | Parse failure / pages inside compressed object streams / custom size | **Conservative no-change**; uses original logic |

When correction is enabled, `-dFIXEDMEDIA -dPDFFitPage` are injected to remap the page to the
**closest standard paper by aspect ratio**, and `-r<level-DPI>` resets the resolution baseline so
Ghostscript can compute the true DPI and downsample normally. The target size **preserves the
original aspect ratio** (long edge maps to paper long edge, short edge is scaled proportionally),
preventing ~6% stretch distortion that would happen if a 4:3 photo were forced into A4.

### Grouping and reproducibility (fixed in v2.3.0)

Geometry-fix arguments are **process-wide** in Ghostscript: every file handled by one process
shares a single page size. Files must therefore be sorted into "geometry groups" first, and only
members of the same group may share a process. Two things must hold:

1. **The probe result must be deterministic.** The main page size used to be picked by ranging
   over a map for the "most frequent" size — but Go randomises map iteration order, so when
   several sizes **tie** (very common in mixed-layout documents, e.g. 2 pages with 2 distinct
   sizes) the winner was random, and the *same file* could be classified differently on different
   runs. It is now selected from an **explicitly sorted list** (most pages first; on a tie, the
   larger long edge wins), so results are fully reproducible.
2. **The group key must include the target page size**, not just the paper name. Since the target
   size is derived from the **original aspect ratio**, two A4-classified scans with different
   source proportions (a 4:3 phone photo vs a √2 scanner) get different targets; keying on the
   paper name alone forced them to share `chunk[0]`'s page size — adding spurious white margins to
   one and making the compression ratio depend on "who the neighbour is".

Measured before the fix: running the same 89-file corpus twice produced **4 files with different
sizes** (`test (64)` differed by 771 KB, ~2.2×), and batch mode disagreed with single-file mode
(`test (1)` by 115 KB). After the fix: two consecutive runs are **byte-for-byte the same size for
87/88 files** (one file shows an 8-byte document-ID jitter, which is Ghostscript's own behaviour),
and **batch mode matches single-file mode exactly** (differing only by the output path length
embedded in the metadata).

### Measured results

- 89 files / 948 MB real corpus: **83 standard (unchanged) / 4 pixel-as-point (fixed) / 2
  conservative (unchanged)**.
- The 4 fixed files now diverge across levels (previously almost identical):

  | File | Source | screen | **ebook (default)** | printer |
  | --- | --- | --- | --- | --- |
  | test (1) | 18.39 MB | 188 KB | **753 KB** | 3.1 MB |
  | test (63) | 24.51 MB | 285 KB | **1425 KB** | 1425 KB |
  | test (64) | 23.33 MB | 108 KB | **337 KB** | 1091 KB |
  | test (104) | 4.75 MB | 31 KB | **113 KB** | 290 KB |

- **Page aspect matches the source**: `test (1)` is 4:3 and now produces a 4:3 page (previously it
  inherited its neighbour's √2 page size and gained letterbox margins). Batch mode and single-file
  mode now agree.
- **Zero impact on standard A4**: the same official document produces 271,669 bytes before and
  after; the only byte differences are timestamps and document IDs, and the xref offset table is
  byte-identical.
- **Aspect deviation ≤ 0.04%** (pure rounding error).
- Visual check: at the `ebook` level (~1/24 the size) titles and body text remain perfectly
  readable.

> This feature is **not applied to pure-text PDFs** (no images means no benefit, and rewriting
> the page structure can actually increase size slightly).

---

## Image downsampling threshold fix (making the level DPI a real cap)

### The problem: why do standard-A4 scans also refuse to compress?

Scans with a normal A4 page size but **higher-resolution images** often show a compression ratio
of ~0% at the default `ebook` (150 DPI) level. Two causes, both meaning the level DPI was never
actually enforced:

1. **`DownsampleThreshold` defaults to 1.5**, i.e. downsampling only happens when
   "source resolution > target × 1.5". For a 200 DPI scan at the 150 DPI level:
   `150 × 1.5 = 225 > 200` → **not a single pixel is dropped**.
2. **JPEG pass-through** (`-dPassThroughJPEGImages=true`) then copies the untouched image
   straight into the output, which ends up a few dozen to a few hundred bytes *larger* than the
   source — the safety net marks it "not smaller" and keeps the original file. The user sees a
   **0% compression ratio**.

### The fix

Lower the downsampling threshold to **1.0** in the base arguments (downsample whenever the
source exceeds the target) and **disable JPEG pass-through** (force re-encoding at the target
resolution):

```
-dColorImageDownsampleThreshold=1.0
-dGrayImageDownsampleThreshold=1.0
-dMonoImageDownsampleThreshold=1.0
-dPassThroughJPEGImages=false
```

Only then do `screen`=72 / `ebook`=150 / `printer`=300 DPI actually mean what they say.

### Measured results

- 8 "won't compress" samples (200 DPI scans, A4): at `ebook` the result went from
  **0% for all 8 files** to **7 of 8 effectively compressed, 42.2% overall**
  (`test(2)` 2.68 MB→0.64 MB, `test(8)` 651 KB→170 KB).
- Full corpus of 89 files / 948 MB: **947.40 MB → 169.70 MB (-82.1%)** versus
  179.75 MB (-81.0%) before. **12 files lost 10.0 MB in total; the other 76 changed only by
  ≤118 bytes (timestamps)** — **zero size regressions**. Runtime 4m04s → 4m31s (+11%).
- Quality (rendered at 150 DPI, pixel-by-pixel): `test(2)` MAE 3.5/255, `test(8)` MAE 3.0/255
  (visually indistinguishable); a **dense small-text table scan** (`test(1)`) shows MAE ≈ 12/255,
  the inherent cost of 200→150 DPI downsampling — use `printer` for such files if fidelity matters.

---

## Safety guard for abnormal files (encrypted / structurally damaged)

For **encrypted PDFs** and **structurally damaged PDFs**, Ghostscript exits with **code 0**
while writing a tiny PDF containing only blank pages (~2–3 KB). Judging success by size alone
would report it as `OK 100%` — and in `--overwrite` mode it would replace the original with a
blank file, causing **irreversible data loss**.

The tool therefore detects Ghostscript's fatal signatures, marks the file as failed, and
**keeps the original**:

| Input | Result |
| --- | --- |
| Encrypted PDF (password required) | `FAIL` — file is encrypted, a password is required (original kept) |
| Damaged / unparseable structure | `FAIL` — cannot parse the source file (original kept) |
| Non-PDF (renamed impostor) | `FAIL` (Ghostscript error) |
| Empty file (0 bytes) | `KEEP` (left untouched) |

No partial or blank artifacts are left behind in the output directory for failed files.

> Note: a PDF that is only mildly non-conformant but that Ghostscript can read in full
> (all pages parsed) is still compressed normally; only files that cannot be read at all fail.

---

## Stall guard (no-progress timeout)

Some heavily damaged PDFs send Ghostscript into a long recovery scan, showing up as the
program producing **no output for a long time** — users reasonably assume the whole run has hung.

### Why a total-runtime timeout does not work

Large files are simply slow: a 1 GB scanned document legitimately takes several minutes.
A fixed total-time limit (say 3 minutes) would **kill such files by mistake**.

### The criterion is "is it making progress", not "how long has it run"

Ghostscript prints a page-progress line (`Page N`) as it finishes each page. So the tool
watches the **interval between outputs**:

- As long as output keeps flowing, the file is progressing and is **never interrupted**,
  no matter how long it takes;
- Once there is **no output at all** for `-timeout` seconds (default 180 s), the file is
  treated as stalled and its process is terminated.

A 1 GB file is therefore never mis-killed, while a genuinely stuck file is caught within
about three minutes.

### What happens on timeout

A stall verdict **never destroys data**:

1. Terminate the Ghostscript process for that file;
2. Remove any partial output;
3. **Keep the original file** (never replaced, even in `--overwrite` mode);
4. Report `FAIL` and move on — **other files keep processing**.

So even in the unlikely event of a misjudgement, the worst case is "this file was not
compressed". **Data loss is not possible.** An unusually slow file can be re-run on its own,
or the guard can be relaxed or disabled:

```bash
pdfcompress -dir <dir> -timeout 0     # disable the stall guard
pdfcompress -dir <dir> -timeout 600   # relax it to 10 minutes
```

> Note: to obtain the page-progress signal, `-dQUIET` is no longer passed to Ghostscript.
> That flag only suppresses progress and the startup banner; it **does not affect the
> compression result** (measured: old and new builds render page-for-page identically —
> only the PDF metadata records one fewer command-line argument) and does not change how
> abnormal files are judged.

---

## Why `-dPDFSTOPONERROR` is not enabled

`-dPDFSTOPONERROR=true` was considered as a "fail fast on broken files" option and was
**rejected after measurement**:

| File type | Off (current) | With `-dPDFSTOPONERROR` |
| --- | --- | --- |
| Fully valid PDF | Compressed normally | Byte-for-byte identical (nothing to trigger) |
| PDF with recoverable page errors | Ghostscript repairs and **carries on**; output essentially complete | Aborts **remaining rendering** on first error → bad page and everything after turn blank; **exit code is still 0**, so the guard misjudges it as success |
| Encrypted / badly damaged | Already caught by the safety guard; kept as original | Also caught by the guard — **no extra benefit** |

The verdict is **~zero benefit, non-zero risk**: fatal errors are already caught precisely by
the text-signature guard, which does not depend on this flag — but the flag silently
upgrades "recoverable bad page" into "silently dropped content", which you cannot even
detect (exit code 0, non-empty output, smaller file — it looks like success). Its behaviour
also differs across Ghostscript versions (some discard only the current page, others
terminate the process), which conflicts with a predictable, identical behaviour on all
three platforms.

The real problem to solve is a **hung process**, and that is handled by the stall guard
above: it judges from outside the process, does not depend on Ghostscript version
behaviour, and its failure mode is observable and reversible.

---

## Supported systems (all fully offline)

| Platform | Architecture | Offline package | Requirements |
| --- | --- | --- | --- |
| Windows | x86 (32-bit) | `pdfcompress-windows-x86.zip` | **Windows 7 / 8 / 10 / 11**, both 32-bit and 64-bit (via WoW64) |
| Linux | ARM64 (aarch64) | `PDFCompress-linux-arm64.tar.xz` | Kylin V10 / UOS etc. **Bundled portable Ghostscript — no install, no network** |
| Linux | x86_64 (amd64) | `PDFCompress-linux-x86_64.tar.xz` | Same as above |

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
upx -9 pdfcompress.exe      # 2.09 MB → 1.04 MB (icon + version info included)
upx -9 pdfcompress          # 1.77 MB → 0.66 MB (arm64)
```

> UPX packing can trigger false positives in some antivirus products — validate whitelisting
> before deployment. Packing is lossless and reversible: `upx -d` restores a byte-identical
> file.

### Adding an icon and version info to the Windows build

The Windows icon and the **Properties → Details** tab come from `winres.syso` in the package
directory: Go automatically links any `*.syso` (a COFF object file) found there when building
for Windows. Both helper scripts are **pure Python with no external dependencies** (no
windres / mingw / go-winres), so they work in offline environments:

```bash
# 1) PNG -> multi-size ICO (16/24/32/48/64/128/256)
python png2ico.py pdf.png app.ico

# 2) ICO + version info -> winres.syso
python make_syso.py winres.syso app.ico 2.1.0

# 3) Normal whole-package build; the resources are embedded automatically
CGO_ENABLED=0 GOOS=windows GOARCH=386 \
  go build -trimpath -buildvcs=false -ldflags "-s -w" -o pdfcompress.exe
```

To change the company name, product name, description, etc., edit the `info` dict inside
`main()` in `make_syso.py`.

> The generated object is 32-bit (i386) COFF and is only valid for `GOARCH=386` Windows
> builds. The Linux builds neither need nor should include `winres.syso`.

---

## Dependencies

- [Ghostscript](https://www.ghostscript.com/) — PDF rewriting engine (bundled in Linux packages)
- [Ghostscript](https://www.ghostscript.com/) — the PDF rewriting engine (bundled in the
  Linux offline packages)
- **No other third-party dependencies** — geometry probing, argument assembly and batch
  scheduling are pure Go standard library; `go.mod` only declares the module and Go version.

## Author

<https://github.com/playgitboy>

## License

This project is intended for internal office use and learning. Please comply with applicable
laws and software licenses. Make sure you have the right to process the target documents, and
keep backups of your originals.
