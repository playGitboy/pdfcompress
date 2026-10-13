# PDF 批量压缩工具 (pdfcompress)

[English](README.en.md) | **简体中文**

一个基于 Go + Ghostscript 的**离线 PDF 批量压缩命令行工具**。单文件二进制、零依赖安装，支持
**多文件与整个目录拖放**，可在**完全离线**的内网与办公终端上运行。

底层调用 Ghostscript 的 `pdfwrite` 设备重写 PDF 流，通过**图片降采样**与**字体内嵌子集化**
两条路径大幅压缩体积，因而对**图片型（扫描件）**和**文字型（电子公文）**两类 PDF 都有显著效果。

---

## 主要功能

- **批量处理**：一次可传入多个文件或整个目录；`-dir` + `-r` 递归处理子目录。
- **拖放即用**：把若干 PDF 文件（或文件夹）拖到程序图标上即可开始压缩，无需敲命令。
- **四级压缩**：`screen` / `ebook` / `printer` / `prepress`，在体积与清晰度之间自由权衡。
- **并发加速**：默认并发数 = CPU 核数，配合单进程批量复用，77 个文件 / 593 MB 实测约 16 秒。
- **安全覆盖**：`-overwrite` 先写临时文件、成功后再原子替换；**压缩后若未变小则保留原文件**，
  绝不放大、绝不损坏原件。
- **失败兜底**：整块校验 + 逐文件重做 + 自动降级，任一步产物缺失都会重处理，不会把半成品当成功。
- **离线自包含**：内置便携 Ghostscript（Linux 版），目标机无需预装任何软件、无需联网。
- **中文友好**：Windows 下双击运行时，出错也会**暂停等待回车**，不会一闪而过看不到报错。

---

## 使用方式

### 方式一：拖放（推荐给日常办公用户）

**Windows**

1. 解压离线包，得到 `pdfcompress.exe` 与同级 `gs\` 目录（**两者必须放在一起**）。
2. 选中一个或多个 PDF 文件，或直接选中一个文件夹。
3. 把它们拖到 `pdfcompress.exe` 图标上，松开即可开始压缩。
4. 输出文件与源文件同目录，命名为 `原文件名_ebook.pdf`（`_ebook` 即当前压缩级别）。

**Linux（麒麟 / 统信等 ARM64、x86_64）**

1. 解压 `PDFCompress-linux-<架构>.tar.gz`（**务必用 tar 解压，不要用图形界面复制**，
   否则可执行权限会丢失）。
2. 图形界面下把 PDF 拖到 `pdfcompress` 文件图标上；或直接执行
   `./pdfcompress 文件1.pdf 文件2.pdf`。

> 拖放为「无参数启动」，等价于下面的批量模式，输出为 `原文件名_<级别>.pdf`。

### 方式二：命令行

```bash
# 压缩单个文件
pdfcompress -i 输入.pdf -o 输出.pdf

# 批量处理目录（默认在输入目录下创建 compressed/ 子目录）
pdfcompress -dir ./待处理

# 递归处理子目录，指定级别与并发数
pdfcompress -dir ./待处理 -r -level printer -j 8

# 覆盖原文件（先写临时文件，成功后替换；未变小时保留原文件）
pdfcompress -dir ./待处理 -overwrite

# 指定 Ghostscript 路径（默认自动查找内置 gs）
pdfcompress -dir ./待处理 -gs "C:\gs\bin\gswin32c.exe"
```

### 命令行参数

| 参数 | 说明 | 默认值 |
| --- | --- | --- |
| `-i` | 输入 PDF 文件路径（单文件模式） | — |
| `-o` | 输出 PDF 路径（单文件模式，与 `-overwrite` 互斥） | — |
| `-dir` | 输入目录（批量模式） | — |
| `-out` | 输出目录（批量模式） | 输入目录下 `compressed/` |
| `-r` | 递归处理子目录 | `false` |
| `-level` | 压缩级别：`screen` / `ebook` / `printer` / `prepress` | `ebook` |
| `-j` | 并发工作数 | CPU 核数（上限 16） |
| `-gsThreads` | 每个 Ghostscript 进程的渲染线程数 | `0`（按并发数自动） |
| `-persist` | 批量模式复用单个 Ghostscript 进程（显著提速） | `true` |
| `-chunk` | 单进程处理的文件数 | `0`（自动） |
| `-overwrite` | 结果覆盖原文件（未变小时保留原文件） | `false` |
| `-gs` | 指定 Ghostscript 路径 | 自动查找 |
| `-q` | 静默模式 | `false` |
| `-version` | 显示版本信息 | — |

### 压缩级别怎么选

| 级别 | 图片降采样 | 典型用途 |
| --- | --- | --- |
| `screen` | 72 DPI | 体积最小；**中文小字会发糊，不建议用于公文** |
| `ebook` | 150 DPI | **默认**。屏幕阅读，体积与清晰度的最佳平衡 |
| `printer` | 300 DPI | 需要打印时使用，体积约为 ebook 的 3 倍 |
| `prepress` | 300 DPI + 单色 1200 DPI | 印刷级，体积最大 |

**两类 PDF 的实际表现（实测）：**

- **文字型（电子公文，几乎不含图片）**：体积 99% 以上来自未子集化的内嵌 CJK 字体，
  压缩主力是**字体子集化**。实测 15.43 MB → 约 220 KB（**-98.6%**），且
  **四个级别输出几乎逐字节相同**——这是正常现象，不是程序异常。
- **图片型（扫描件）**：四个级别差异明显。源图分辨率高于目标 DPI 时才真正降采样；
  源图分辨率已经低于目标 DPI 时，图片会**无损直通**（所以 `printer` 有时反而更快、更大）。

---

## 支持的系统（均可离线使用）

| 平台 | 架构 | 离线包 | 运行要求 |
| --- | --- | --- | --- |
| Windows | x86（32 位） | `pdfcompress-windows-x86.zip` | **Windows 7 / 8 / 10 / 11**，32 位与 64 位系统均可（WoW64） |
| Linux | ARM64 (aarch64) | `PDFCompress-linux-arm64.tar.gz` | 麒麟 V10 / 统信 UOS 等，**内置便携 Ghostscript，免安装免联网** |
| Linux | x86_64 (amd64) | `PDFCompress-linux-x86_64.tar.gz` | 同上 |

- **完全离线**：所有离线包均已包含运行所需的全部文件，目标机无需安装 Ghostscript、
  无需联网、无需 Node/Python 等任何运行时。
- **Windows 版为何只有 32 位**：为兼容 Windows 7，编译使用 Go 1.19；单一 x86 版本可
  通过 WoW64 同时运行在 32 / 64 位系统上，部署最简单。
- **Linux 版内置 Ghostscript**：采用 musl 静态链接版本，与发行版 glibc 解耦，
  不受系统 Ghostscript 版本影响；主程序会自动优先使用同目录下的 `gs/`。

---

## 从源码构建

需要 **Go 1.19.x**（更高的 Go 版本将无法在 Windows 7 上运行）。

```bash
# Windows x86（兼容 Win7 的 32 位版本）
CGO_ENABLED=0 GOOS=windows GOARCH=386 \
  go build -trimpath -buildvcs=false -ldflags "-s -w" -o pdfcompress.exe

# Linux ARM64（麒麟 / 统信）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -buildvcs=false -ldflags "-s -w" -o pdfcompress

# Linux x86_64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -ldflags "-s -w" -o pdfcompress
```

> 注意：必须 **整包编译**（在项目根目录执行 `go build .`），
> 否则 `console_windows.go` 等平台文件不参与构建，双击检测会失效。

如需进一步缩小体积，可用 UPX 加壳：

```bash
upx -9 pdfcompress.exe      # 1.81 MB → 0.72 MB
upx -9 pdfcompress          # 1.77 MB → 0.66 MB (arm64)
```

> UPX 加壳可能被部分国内杀毒软件误报，正式部署前请先做白名单验证。
> 加壳是无损可逆的：`upx -d` 解包后与原文件逐字节一致。

---

## 依赖

- [Ghostscript](https://www.ghostscript.com/) — PDF 重写引擎（Linux 离线包已内置）
- [pdfcpu](https://github.com/pdfcpu/pdfcpu) v0.15.0 — PDF 结构校验

## 作者

<https://github.com/playgitboy>

## 许可

本项目仅供内部办公与学习交流使用，请遵守所在地法律法规及软件许可协议。
使用时请自行确认对目标文档拥有处理权限，并注意做好原始文件备份。
