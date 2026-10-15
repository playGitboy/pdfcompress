package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	version = "2.1.0"
	author  = "https://github.com/playgitboy"
)

// launchedByDoubleClick 判断本次运行是否由「双击图标 / 拖放文件到图标」启动。
//
// 只有这种情况才需要在结束时等待回车，让用户看清输出；
// 从命令提示符、终端或脚本中调用时应直接退出，不打断自动化流程。
//
// 默认实现恒为 false（即不等待）；Windows 平台文件会在 init() 中
// 注入真实检测（GetConsoleProcessList），这样即使单独编译 main.go 也不会出错。
var launchedByDoubleClick = func() bool { return false }

// 压缩级别。内置档位用 iota（0-3）；自定义 DPI 用 1000+dpi 编码。
type CompressLevel int

const (
	LevelScreen   CompressLevel = iota // 72 DPI，最小体积
	LevelEBook                         // 150 DPI，屏幕阅读（默认）
	LevelPrinter                       // 300 DPI，打印质量
	LevelPrepress                      // 300 DPI+，高质量
)

// customLevelBase 用于编码自定义 DPI 值，避免与内置 iota 冲突。
const customLevelBase = 1000

func parseLevel(s string) CompressLevel {
	switch strings.ToLower(s) {
	case "screen":
		return LevelScreen
	case "ebook":
		return LevelEBook
	case "printer":
		return LevelPrinter
	case "prepress":
		return LevelPrepress
	default:
		// 尝试解析为自定义 PPI/DPI 数值（如 "100"）
		if dpi, err := strconv.Atoi(s); err == nil && dpi >= 10 && dpi <= 2400 {
			return CompressLevel(customLevelBase + dpi)
		}
		return LevelEBook
	}
}

func levelName(l CompressLevel) string {
	switch l {
	case LevelScreen:
		return "screen (72 DPI，最小体积)"
	case LevelEBook:
		return "ebook (150 DPI，屏幕阅读推荐)"
	case LevelPrinter:
		return "printer (300 DPI，打印质量)"
	case LevelPrepress:
		return "prepress (300 DPI+，出版级)"
	default:
		if l >= customLevelBase {
			return fmt.Sprintf("custom (%d DPI，用户自定义)", int(l)-customLevelBase)
		}
		return "unknown"
	}
}

// levelSlug 返回用于输出文件名的压缩级别短名称（screen/ebook/printer/prepress/customXX）
func levelSlug(l CompressLevel) string {
	switch l {
	case LevelScreen:
		return "screen"
	case LevelEBook:
		return "ebook"
	case LevelPrinter:
		return "printer"
	case LevelPrepress:
		return "prepress"
	default:
		if l >= customLevelBase {
			return fmt.Sprintf("custom%d", int(l)-customLevelBase)
		}
		return "compressed"
	}
}

// gsSettings 返回各压缩级别独有的图像分辨率参数。
//
// 注意：这里刻意不使用 -dPDFSETTINGS=/xxx。实测在含图片的 PDF 上，
// -dPDFSETTINGS 会让 Ghostscript 走进一条低效路径，耗时约为不用它的
// 6~8 倍（同一文件 1.3s → 9.3s），而输出体积只差约 1%、清晰度完全一致；
// 对纯文字 PDF 更是毫无作用。因此改为显式指定分辨率，速度与体积兼得。
//
// -dJPEGQ=xx 亦已移除：它不是 pdfwrite 的合法参数，实测无论取值多少
// 输出字节完全相同（无任何效果），保留只会误导。
func gsSettings(level CompressLevel) []string {
	// 内置级别
	switch level {
	case LevelScreen:
		return []string{
			"-dColorImageResolution=72",
			"-dGrayImageResolution=72",
			"-dMonoImageResolution=300",
		}
	case LevelEBook:
		return []string{
			"-dColorImageResolution=150",
			"-dGrayImageResolution=150",
			"-dMonoImageResolution=300",
		}
	case LevelPrinter:
		return []string{
			"-dColorImageResolution=300",
			"-dGrayImageResolution=300",
			"-dMonoImageResolution=600",
		}
	case LevelPrepress:
		return []string{
			"-dColorImageResolution=300",
			"-dGrayImageResolution=300",
			"-dMonoImageResolution=1200",
		}
	}
	// 自定义 DPI（10-2400）
	if level >= customLevelBase {
		dpi := int(level) - customLevelBase
		mono := dpi * 2
		if mono < 300 {
			mono = 300
		}
		if mono > 2400 {
			mono = 2400
		}
		return []string{
			fmt.Sprintf("-dColorImageResolution=%d", dpi),
			fmt.Sprintf("-dGrayImageResolution=%d", dpi),
			fmt.Sprintf("-dMonoImageResolution=%d", mono),
		}
	}
	// 默认 ebook
	return []string{
		"-dColorImageResolution=150",
		"-dGrayImageResolution=150",
		"-dMonoImageResolution=300",
	}
}

// 各模式共用的基础参数
// 速度：跳过页面自动旋转分析；已达目标分辨率的图像直接透传，避免无谓重编码
// 体积：图像降采样 + 字体子集化 + JPEG 编码
// 兼容：不使用 -dPDFSETTINGS（见 gsSettings 注释）
func baseGSArgs() []string {
	return []string{
		"-sDEVICE=pdfwrite",
		"-dCompatibilityLevel=1.4",
		"-dNOPAUSE",
		"-dQUIET",
		"-dBATCH",
		"-dAutoRotatePages=/None",
		"-dPassThroughJPEGImages=true",
		"-dPassThroughJPXImages=true",
		"-dDetectDuplicateImages=true",
		"-dEmbedAllFonts=true", // 文字型PDF（内嵌大字体）压缩主力：子集化+嵌入
		"-dCompressFonts=true",
		"-dSubsetFonts=true",
		"-dDownsampleColorImages=true",
		"-dDownsampleGrayImages=true",
		"-dDownsampleMonoImages=true",
		"-dColorImageDownsampleType=/Bicubic",
		"-dGrayImageDownsampleType=/Bicubic",
		"-dMonoImageDownsampleType=/Subsample", // 1bit 图像用 Subsample（Bicubic 不适用且更慢）
		"-dAutoFilterColorImages=false",
		"-dAutoFilterGrayImages=false",
		"-dColorImageFilter=/DCTEncode",
		"-dGrayImageFilter=/DCTEncode",
	}
}

// geomGroupKey 把几何探测结果映射为「压缩策略分组键」。
//
// 同一分组的文件可以放进同一个 gs 进程；不同分组必须分开，
// 因为几何基准修正参数是进程级的（见 compressChunk 注释）。
//
//	"std"        —— 标准纸张或无法判定：沿用原有参数，"行为与旧版完全一致"
//	"fix:A4" 等  —— 像素当 pt 扫描件：启用 FIXEDMEDIA/FitPage/-r 修正
func geomGroupKey(g pdfGeom) string {
	if !g.shouldFixGeometry() {
		return "std"
	}
	if g.paper.name == "" {
		return "fix:A4"
	}
	return "fix:" + g.paper.name
}

// 用于「几何基准修正」时通过 -r 重设 gs 的 DPI 基准。
func levelDPI(level CompressLevel) int {
	switch level {
	case LevelScreen:
		return 72
	case LevelEBook:
		return 150
	case LevelPrinter, LevelPrepress:
		return 300
	}
	if level >= customLevelBase {
		return int(level) - customLevelBase
	}
	return 150
}

// geometryArgs 针对「像素当 pt」的扫描件，返回几何基准修正所需的额外参数。
//
// 背景见 pdfgeom.go 顶部注释：这类 PDF 的页面尺寸等于图片像素数，
// gs 会把图像有效 DPI 算成 72，从而判定「无需降采样」，
// 导致所有档位输出几乎相同、压缩率接近 0。
//
// 修正方式：
//   - -dFIXEDMEDIA -dPDFFitPage：把页面重设为合理物理尺寸，
//     让 gs 以真实物理尺寸重新计算图像 DPI；
//   - -dDEVICEWIDTHPOINTS/-dDEVICEHEIGHTPOINTS：目标页面尺寸，
//     按原始长宽比换算（见 targetPageSize），避免比例拉伸；
//   - -r<档位DPI>：把基准 DPI 设为档位值，与 -dColorImageResolution 配合；
//   - -dPassThroughJPEGImages=false：强制重编码。
//     像素当 pt 的扫描件其原始 JPEG 往往已是「重编码过一次」的高质量样本，
//     若仍允许透传，gs 会原样搬运、不做任何压缩。
//
// 若该文件不需要修正（标准纸张/无图像/无法判定），返回 nil，调用方行为完全不变。
func geometryArgs(g pdfGeom, level CompressLevel) []string {
	if !g.shouldFixGeometry() {
		return nil
	}
	dpi := levelDPI(level)
	pw, ph := targetPageSize(g)
	return []string{
		"-dFIXEDMEDIA",
		"-dPDFFitPage",
		fmt.Sprintf("-dDEVICEWIDTHPOINTS=%.2f", pw),
		fmt.Sprintf("-dDEVICEHEIGHTPOINTS=%.2f", ph),
		fmt.Sprintf("-r%d", dpi),
		"-dPassThroughJPEGImages=false",
	}
}

func formatSize(bytes int64) string {
	const unit = 1024.0
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// 输出文件是否有效（存在且非空）
func validOutput(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// sOutputFileValue 处理 -sOutputFile= 取值：
// 反斜杠统一为正斜杠；% 需转义为 %%（gs 把 %d/%s 当作输出文件名模板）
func sOutputFileValue(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	p = strings.ReplaceAll(p, "%", "%%")
	return p
}

// psQuote 把路径转成 PostScript 字符串字面量
// 反斜杠转正斜杠（Windows 路径 gs 同样接受）；转义 ( ) \ 三个特殊字符
func psQuote(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	var b strings.Builder
	b.WriteByte('(')
	for _, r := range p {
		switch r {
		case '(', ')', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte(')')
	return b.String()
}

// truncName 按字符（而非字节）截断文件名，避免把多字节中文切成乱码
func truncName(s string, max int) string {
	r := []rune(s)
	if len(r) <= max || max < 4 {
		return s
	}
	return "..." + string(r[len(r)-(max-3):])
}

// gsErr 把 Ghostscript 的冗长报错压缩成一行，便于阅读
func gsErr(err error, out string) error {
	msg := strings.TrimSpace(out)
	if i := strings.IndexAny(msg, "\r\n"); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	if msg == "" {
		return err
	}
	return fmt.Errorf("%v: %s", err, msg)
}

// withSep 保证目录路径以分隔符结尾（gs 的 --permit-file-* 用目录时要求如此）
func withSep(dir string) string {
	dir = strings.ReplaceAll(dir, `\`, "/")
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	return dir
}

// 覆盖模式下的临时输出路径。
// 必须与源文件同目录（同盘才能原子替换）；扩展名为 .tmp，
// 以免被扫描逻辑误当成待处理 PDF；名字尽量短，规避 Windows 260 字符路径上限。
func tempOutputPath(dir string, idx int) string {
	return filepath.Join(dir, fmt.Sprintf(".pdfc%d_%d.tmp", os.Getpid(), idx))
}

// 用压缩结果替换原文件。
// Go 在 Windows 上的 os.Rename 使用 MOVEFILE_REPLACE_EXISTING，可直接覆盖；
// 但目标是只读文件时会失败，此时清除只读位后重试一次。
func replaceOriginal(tmp, dst string) error {
	err := os.Rename(tmp, dst)
	if err == nil {
		return nil
	}
	if info, statErr := os.Stat(dst); statErr == nil {
		if chErr := os.Chmod(dst, info.Mode().Perm()|0200); chErr == nil {
			if retryErr := os.Rename(tmp, dst); retryErr == nil {
				return nil
			}
		}
	}
	return err
}

// 查找Ghostscript
// Windows: 统一使用32位版本(x86)，32位和64位Windows(含Win7)都能运行
// Linux(麒麟/统信UOS等): 优先程序同目录gs/bin/gs，其次系统PATH中的gs
func findGhostscript() (string, error) {
	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)

	var candidates []string
	var pathNames []string
	if runtime.GOOS == "windows" {
		candidates = []string{
			filepath.Join(exeDir, "gs", "bin", "x86", "gswin32c.exe"),
			filepath.Join(exeDir, "gs", "bin", "gswin32c.exe"), // 兼容旧布局
		}
		pathNames = []string{"gswin32c", "gswin64c", "gs"}
	} else {
		candidates = []string{
			filepath.Join(exeDir, "gs", "bin", "gs"),
			filepath.Join(exeDir, "gs", "gs"),
		}
		pathNames = []string{"gs"}
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	for _, name := range pathNames {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}

	if runtime.GOOS == "windows" {
		return "", fmt.Errorf("未找到Ghostscript。请确保程序目录下存在 gs\\bin\\x86\\gswin32c.exe")
	}
	return "", fmt.Errorf("未找到Ghostscript(gs)。请先安装: 麒麟Kylin执行 sudo yum install ghostscript；统信UOS执行 sudo apt install ghostscript")
}

// detectGSVersion 读取 Ghostscript 主/次版本号
// 用途：--permit-file-read/write 需要 gs >= 9.50（该版本起 SAFER 默认开启）
func detectGSVersion(gsExe string) (major, minor int, ok bool) {
	out, err := exec.Command(gsExe, "--version").Output()
	if err != nil {
		return 0, 0, false
	}
	s := strings.TrimSpace(string(out))
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	maj, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	min, e2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// 压缩单个PDF（每个文件一个 gs 进程）
func compressFile(gsExe, input, output string, level CompressLevel, gsThreads int) error {
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %v", err)
	}

	// 几何探测：仅对「像素当 pt」的扫描件启用基准修正，其余返回 nil、行为不变
	geom := probePDFGeom(input)

	args := baseGSArgs()
	if gsThreads > 1 {
		args = append(args, fmt.Sprintf("-dNumRenderingThreads=%d", gsThreads))
	}
	args = append(args, geometryArgs(geom, level)...)
	args = append(args, gsSettings(level)...)
	args = append(args, fmt.Sprintf("-sOutputFile=%s", sOutputFileValue(output)), input)

	cmd := exec.Command(gsExe, args...)
	cmd.Stdin = nil
	outputBytes, err := cmd.CombinedOutput()
	if err != nil {
		return gsErr(err, string(outputBytes))
	}
	if !validOutput(output) {
		return fmt.Errorf("压缩失败，未生成输出文件")
	}
	return nil
}

// 用「一个 gs 进程」连续压缩多个文件。
// 原理：gs 进程启动约 300ms（加载 gsdll 等），而单文件净处理仅数百毫秒，
// 把多个文件交给同一进程处理可省掉绝大部分启动开销。
// 通过 PostScript 的 setpagedevice 切换 /OutputFile，实现每个文件独立输出。
//
// ⚠ 调用方保证：同一 chunk 内所有文件的 geomKey 必须相同。
// 因为几何基准修正（-dFIXEDMEDIA/-r 等）是页面设备级参数，
// 一个进程内无法按文件切换。不同几何类别的文件必须分到不同 chunk。
func compressChunk(gsExe string, inputs, outputs []string, level CompressLevel,
	gsThreads int, permitRead, permitWrite []string, geom pdfGeom) error {

	if len(inputs) == 0 {
		return nil
	}
	if len(inputs) == 1 {
		return compressFile(gsExe, inputs[0], outputs[0], level, gsThreads)
	}

	for _, o := range outputs {
		if err := os.MkdirAll(filepath.Dir(o), 0755); err != nil {
			return fmt.Errorf("创建输出目录失败: %v", err)
		}
	}

	args := baseGSArgs()
	if gsThreads > 1 {
		args = append(args, fmt.Sprintf("-dNumRenderingThreads=%d", gsThreads))
	}
	// 几何基准修正（仅「像素当 pt」扫描件组；标准纸张组为 nil，行为不变）
	args = append(args, geometryArgs(geom, level)...)
	// SAFER 沙箱授权（gs>=9.50 默认开启 SAFER，切换输出与 run 输入都需显式放行）
	for _, d := range permitRead {
		args = append(args, "--permit-file-read="+d)
	}
	for _, d := range permitWrite {
		args = append(args, "--permit-file-write="+d)
	}
	args = append(args, gsSettings(level)...)
	// 设备以第一个输出文件打开
	args = append(args, "-sOutputFile="+sOutputFileValue(outputs[0]))
	// 第一个文件直接 run
	args = append(args, "-c", psQuote(inputs[0])+" run")
	// 后续文件：先切换输出文件，再 run
	for i := 1; i < len(inputs); i++ {
		args = append(args, "-c",
			fmt.Sprintf("<< /OutputFile %s >> setpagedevice %s run",
				psQuote(outputs[i]), psQuote(inputs[i])))
	}

	cmd := exec.Command(gsExe, args...)
	cmd.Stdin = nil
	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		return gsErr(err, string(outBytes))
	}
	return nil
}

// 查找所有PDF文件
// skip 为需要跳过的目录集合（输出目录），避免二次运行时把上一次的输出当成输入。
// 优化：不再对每个条目调用 filepath.Abs（其内部会触发 os.Getwd 系统调用），
// 改为比较 Clean 后的路径，避免 O(N) 次系统调用。
func findPDFs(root string, recursive bool, skip map[string]bool) ([]string, error) {
	var pdfs []string

	if recursive {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if skip[filepath.Clean(path)] {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.EqualFold(filepath.Ext(path), ".pdf") {
				pdfs = append(pdfs, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".pdf") {
				pdfs = append(pdfs, filepath.Join(root, e.Name()))
			}
		}
	}

	sort.Strings(pdfs) // 固定顺序，便于结果对照与复现
	return pdfs, nil
}

// exitAfterPause 退出程序。若为双击/拖放启动，先等待用户回车，
// 避免出错时控制台窗口一闪而过、用户根本来不及看清错误信息。
func exitAfterPause(code int) {
	if code != 0 {
		pause()
	}
	os.Exit(code)
}

// pause 仅在「双击 / 拖放启动」时等待回车，避免控制台窗口一闪而过；
// 从命令行/终端运行时直接返回，执行完即退出。
func pause() {
	if !launchedByDoubleClick() {
		return
	}
	fmt.Println()
	fmt.Println("按回车键继续...")
	fmt.Scanln()
}

// 压缩单个PDF并输出结果，返回是否成功
func runSingle(gsExe, input, output string, level CompressLevel, overwrite, quiet bool, gsThreads int) bool {
	start := time.Now()
	origSize := fileSize(input)

	writePath := output
	if overwrite {
		writePath = tempOutputPath(filepath.Dir(input), 0)
	}

	if err := compressFile(gsExe, input, writePath, level, gsThreads); err != nil {
		if overwrite {
			os.Remove(writePath)
		}
		fmt.Fprintf(os.Stderr, "压缩失败: %v\n", err)
		return false
	}

	compSize := fileSize(writePath)
	kept := false
	if compSize <= 0 || compSize >= origSize {
		// 压缩后没有变小，保留原文件更划算
		os.Remove(writePath)
		compSize = origSize
		kept = true
	} else if overwrite {
		if err := replaceOriginal(writePath, input); err != nil {
			os.Remove(writePath)
			fmt.Fprintf(os.Stderr, "压缩失败: 替换原文件失败（文件可能被占用）: %v\n", err)
			return false
		}
	}

	ratio := float64(0)
	if origSize > 0 {
		ratio = (1 - float64(compSize)/float64(origSize)) * 100
	}

	if !quiet {
		fmt.Println()
		fmt.Println("=== 压缩完成 ===")
		fmt.Printf("原始大小: %s\n", formatSize(origSize))
		fmt.Printf("压缩后:   %s\n", formatSize(compSize))
		fmt.Printf("压缩率:   %.1f%%\n", ratio)
		fmt.Printf("耗时:     %s\n", time.Since(start).Round(time.Millisecond))
		if kept {
			fmt.Println("输出:     已保留原文件（压缩后未变小）")
		} else if overwrite {
			fmt.Println("输出:     已覆盖原文件")
		} else {
			fmt.Printf("输出:     %s\n", output)
		}
	} else {
		tag := "OK"
		if kept {
			tag = "KEEP"
		}
		fmt.Printf("%s  %s -> %s (%.1f%%)\n", tag, formatSize(origSize), formatSize(compSize), ratio)
	}
	return true
}

// 一个待处理文件
type fileTask struct {
	input     string
	writePath string // gs 实际写入的路径
	finalPath string // 最终文件路径（覆盖模式=原文件；否则=输出文件）
	rel       string
	orig      int64
	overwrite bool

	// geomKey 是几何分组的键：同一键的文件才会被放进同一个 gs 进程。
	//
	// 原因：几何基准修正（-dFIXEDMEDIA/-r/-dPassThroughJPEGImages）是
	// 页面设备级参数，一个 gs 进程内无法为不同文件切换；
	// 若把「标准 A4 公文」与「像素当 pt 扫描件」混在同一进程，
	// 后者的修正会污染前者（把 A4 公文也强制套上 FIXEDMEDIA）。
	geomKey string
	// geom 该文件的几何探测结果（仅当需要修正时用于生成参数）
	geom pdfGeom
}

// 一个文件的处理结果
type fileResult struct {
	task     *fileTask
	compSize int64
	kept     bool // 覆盖模式下因未变小而保留原文件
	err      error
}

// planChunkSize 计算每个 gs 进程应处理的文件数
// 实测结论：chunk 越大越省启动开销，但 chunk 过多会牺牲并行度与负载均衡。
//   - 文件数不超过并发数：每个文件一个进程，吃满多核
//   - 否则：按 2×并发数 切块，块大小限制在 [2,16]
func planChunkSize(n, workers, override int) int {
	if n <= 1 {
		return 1
	}
	if override > 0 {
		if override > n {
			return n
		}
		return override
	}
	if n <= workers {
		return 1
	}
	cs := (n + workers*2 - 1) / (workers * 2)
	if cs < 2 {
		cs = 2
	}
	if cs > 16 {
		cs = 16
	}
	return cs
}

// defaultWorkers 默认并发数：不超过 CPU 核数，并封顶 16。
// 每个 gs 进程峰值内存约 65MB，封顶可避免在超多核机器上一次性拉起过多进程。
func defaultWorkers() int {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	if n > 16 {
		n = 16
	}
	return n
}

// capThreads 限制传给 gs 的渲染线程数（超过 8 收益极小，反而增加调度开销）
func capThreads(n int) int {
	if n < 1 {
		return 1
	}
	if n > 8 {
		return 8
	}
	return n
}

// calcGSThreads 根据并发数自适应分配Ghostscript内部渲染线程：
// 总线程数控制在 CPU 核数附近，避免“外层并发 × 内层线程”造成线程超订。
func calcGSThreads(workers int) int {
	n := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	t := n / workers
	return capThreads(t)
}

// 批量压缩目录中的PDF，返回失败文件数（扫描错误返回-1）
func runBatch(gsExe, inputDir, outputDir string, recursive bool, level CompressLevel,
	workers int, quiet bool, gsThreads int, persist, usePermits bool, chunkOverride int, overwrite bool) int32 {

	start := time.Now()
	absDir, _ := filepath.Abs(inputDir)

	outRoot := outputDir
	if outRoot == "" {
		outRoot = filepath.Join(absDir, "compressed")
	}
	absOut, _ := filepath.Abs(outRoot)
	if workers < 1 {
		workers = 1
	}

	if !quiet {
		fmt.Printf("扫描目录: %s\n", absDir)
		if overwrite {
			fmt.Printf("输出方式: 覆盖原文件（先写临时文件，成功后再替换）\n")
		} else {
			fmt.Printf("输出目录: %s\n", absOut)
		}
		fmt.Printf("递归子目录: %v\n", recursive)
		fmt.Printf("并发数: %d\n", workers)
		fmt.Println()
		fmt.Println("正在扫描PDF文件...")
	}

	// 跳过目录：默认的 compressed 输出目录，以及（若位于输入树内）自定义输出目录
	skipDirs := map[string]bool{
		filepath.Clean(filepath.Join(absDir, "compressed")): true,
	}
	if rel, relErr := filepath.Rel(absDir, absOut); relErr == nil &&
		rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		skipDirs[filepath.Clean(absOut)] = true
	}

	pdfs, err := findPDFs(absDir, recursive, skipDirs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "扫描文件失败: %v\n", err)
		return -1
	}
	if len(pdfs) == 0 {
		fmt.Println("未找到PDF文件")
		return 0
	}

	// 构造任务并预建输出目录（原实现每个文件都 MkdirAll 一次）
	tasks := make([]*fileTask, 0, len(pdfs))
	dirCache := make(map[string]bool, 16)
	for i, p := range pdfs {
		rel, relErr := filepath.Rel(absDir, p)
		if relErr != nil {
			rel = filepath.Base(p)
		}
		t := &fileTask{input: p, rel: rel, orig: fileSize(p), overwrite: overwrite}
		if overwrite {
			t.writePath = tempOutputPath(filepath.Dir(p), i)
			t.finalPath = p
		} else {
			out := filepath.Join(absOut, rel)
			d := filepath.Dir(out)
			if !dirCache[d] {
				_ = os.MkdirAll(d, 0755)
				dirCache[d] = true
			}
			t.writePath = out
			t.finalPath = out
		}
		// 几何探测：决定该文件属于哪个压缩策略分组。
		// 标准纸张/无法判定 → "std"（沿用原参数，行为不变）
		// 像素当 pt 扫描件    → "fix:<纸张>"（启用几何基准修正）
		t.geom = probePDFGeom(p)
		t.geomKey = geomGroupKey(t.geom)
		tasks = append(tasks, t)
	}

	// 规划分块：决定“单进程多文件”还是“一文件一进程”
	chunkSize := planChunkSize(len(tasks), workers, chunkOverride)
	usePersist := persist && usePermits && chunkSize > 1

	if !quiet {
		mode := "每文件独立进程"
		if usePersist {
			mode = fmt.Sprintf("单进程批量（每进程 %d 个文件）", chunkSize)
		}
		fmt.Printf("找到 %d 个PDF文件，开始压缩... [%s]\n\n", len(tasks), mode)
		fmt.Printf("%-5s %-6s %10s %10s %8s  %s\n", "#", "状态", "原始", "压缩后", "压缩率", "文件")
		fmt.Println(strings.Repeat("-", 100))
	}

	// 构建分块：按「几何分组」先归类，再在组内按体积轮转分配。
	//
	// 为什么要先按 geomKey 分组：
	// 几何基准修正参数是 gs 进程级的，一个进程内所有文件共享同一套页面设备参数。
	// 若把「标准 A4 公文」与「像素当 pt 扫描件」放进同一进程，
	// 扫描件所需的 -dFIXEDMEDIA/-r 会同时作用到公文上，破坏其原始版式。
	// 因此分组是正确性要求，不是优化。
	//
	// 组内分配沿用原策略：按体积从大到小排序后轮转分配。
	// 注意不要用“按字节负载最小”的贪心分配——小文件体积小，会全部涌入同一个
	// 负载最小的块，形成极长的尾块。轮转分配可同时均衡文件数与总体积。
	var chunks [][]*fileTask
	if usePersist {
		// 1) 按 geomKey 归组（保持文件顺序稳定，便于复现）
		var groupOrder []string
		groupMap := make(map[string][]*fileTask)
		for _, t := range tasks {
			if _, ok := groupMap[t.geomKey]; !ok {
				groupOrder = append(groupOrder, t.geomKey)
			}
			groupMap[t.geomKey] = append(groupMap[t.geomKey], t)
		}
		// 2) 每组内部分块
		for _, key := range groupOrder {
			grp := groupMap[key]
			numChunks := (len(grp) + chunkSize - 1) / chunkSize
			if numChunks < 1 {
				numChunks = 1
			}
			part := make([][]*fileTask, numChunks)
			order := make([]int, len(grp))
			for i := range order {
				order[i] = i
			}
			sort.SliceStable(order, func(a, b int) bool { return grp[order[a]].orig > grp[order[b]].orig })
			for k, idx := range order {
				c := k % numChunks
				part[c] = append(part[c], grp[idx])
			}
			for _, c := range part {
				if len(c) > 0 {
					chunks = append(chunks, c)
				}
			}
		}
	} else {
		chunks = make([][]*fileTask, len(tasks))
		for i, t := range tasks {
			chunks[i] = []*fileTask{t}
		}
	}

	permitRead := []string{withSep(absDir)}
	permitWrite := []string{withSep(absDir), withSep(absOut)}

	// 结果通道：由主 goroutine 统一打印，统计量无需加锁
	results := make(chan fileResult, 256)
	chunkCh := make(chan []*fileTask, len(chunks))
	for _, c := range chunks {
		chunkCh <- c
	}
	close(chunkCh)

	par := workers
	if par > len(chunks) {
		par = len(chunks)
	}
	if par < 1 {
		par = 1
	}

	var wg sync.WaitGroup
	var persistBroken int32 // 一旦发现本机单进程模式不可用，后续块直接走逐文件

	for w := 0; w < par; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// 后处理：覆盖模式下用临时文件替换原文件；未变小时保留原文件
			finalize := func(t *fileTask) fileResult {
				compSize := fileSize(t.writePath)
				if compSize <= 0 || compSize >= t.orig {
					os.Remove(t.writePath)
					return fileResult{task: t, compSize: t.orig, kept: true}
				}
				if t.overwrite {
					if err := replaceOriginal(t.writePath, t.finalPath); err != nil {
						os.Remove(t.writePath)
						return fileResult{task: t, err: fmt.Errorf("替换原文件失败（可能被占用）: %v", err)}
					}
				}
				return fileResult{task: t, compSize: compSize}
			}
			// 失败：清理残留（临时文件或半成品），避免留下误导性的文件
			failed := func(t *fileTask, e error) fileResult {
				os.Remove(t.writePath)
				return fileResult{task: t, err: e}
			}
			// 逐文件处理一个块（也是单进程模式不可用时的降级路径）
			runPerFile := func(chunk []*fileTask) {
				for _, t := range chunk {
					if e := compressFile(gsExe, t.input, t.writePath, level, gsThreads); e != nil {
						results <- failed(t, e)
						continue
					}
					results <- finalize(t)
				}
			}

			for chunk := range chunkCh {
				if atomic.LoadInt32(&persistBroken) == 1 {
					runPerFile(chunk)
					continue
				}

				inputs := make([]string, len(chunk))
				outputs := make([]string, len(chunk))
				for i, t := range chunk {
					inputs[i] = t.input
					outputs[i] = t.writePath
				}
				// 同 chunk 内 geomKey 必然相同（分块时已按 geomKey 归类），
				// 取第一个任务的几何信息代表整块即可。
				chunkErr := compressChunk(gsExe, inputs, outputs, level, gsThreads,
					permitRead, permitWrite, chunk[0].geom)

				// 整块成功要求“每个文件都产出了有效文件”。只要有任何一个缺失就整块重做，
				// 否则可能把异常产物（例如内容被合并进第一个文件）当作成功结果接受。
				chunkOK := chunkErr == nil
				if chunkOK {
					for _, t := range chunk {
						if !validOutput(t.writePath) {
							chunkOK = false
							break
						}
					}
				}
				if chunkOK {
					for _, t := range chunk {
						results <- finalize(t)
					}
					continue
				}

				// 一个产物都没有 → 判定本机不支持单进程模式（如 gs 版本/构建差异），
				// 后续块直接走逐文件，避免每次都白跑一遍。
				if len(chunk) > 1 {
					anyValid := false
					for _, t := range chunk {
						if validOutput(t.writePath) {
							anyValid = true
							break
						}
					}
					if !anyValid && atomic.CompareAndSwapInt32(&persistBroken, 0, 1) && !quiet {
						fmt.Fprintln(os.Stderr, "[提示] 单进程批量模式在本机不可用，已自动切换为逐文件模式")
					}
				}
				runPerFile(chunk)
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var totalFiles int32 = int32(len(tasks))
	var successFiles int32
	var failFiles int32
	var keptFiles int32
	var totalOrig int64
	var totalComp int64
	var done int32
	failList := make([]string, 0)

	for res := range results {
		done++
		totalOrig += res.task.orig

		displayName := truncName(res.task.rel, 60)

		if res.err != nil {
			failFiles++
			failList = append(failList, fmt.Sprintf("%s: %v", res.task.rel, res.err))
			if !quiet {
				fmt.Printf("%-5d %-6s %10s %10s %8s  %s\n",
					done, "FAIL", formatSize(res.task.orig), "-", "-", displayName)
			}
			continue
		}

		successFiles++
		totalComp += res.compSize
		ratio := float64(0)
		if res.task.orig > 0 {
			ratio = (1 - float64(res.compSize)/float64(res.task.orig)) * 100
		}
		if !quiet {
			tag := "OK"
			if res.kept {
				tag = "KEEP"
				keptFiles++
			}
			fmt.Printf("%-5d %-6s %10s %10s %7.1f%%  %s\n",
				done, tag, formatSize(res.task.orig), formatSize(res.compSize), ratio, displayName)
		} else if res.kept {
			keptFiles++
		}
	}

	elapsed := time.Since(start)
	totalRatio := float64(0)
	if totalOrig > 0 {
		totalRatio = (1 - float64(totalComp)/float64(totalOrig)) * 100
	}

	if !quiet {
		fmt.Println(strings.Repeat("-", 100))
		fmt.Println()
		fmt.Println("=== 压缩完成 ===")
		fmt.Printf("总文件数: %d\n", totalFiles)
		fmt.Printf("成功: %d\n", successFiles)
		fmt.Printf("失败: %d\n", failFiles)
		if overwrite && keptFiles > 0 {
			fmt.Printf("保留原文件（压缩后未变小）: %d\n", keptFiles)
		}
		fmt.Printf("原始总大小: %s\n", formatSize(totalOrig))
		fmt.Printf("压缩后总大小: %s\n", formatSize(totalComp))
		fmt.Printf("总压缩率: %.1f%%\n", totalRatio)
		fmt.Printf("总耗时: %s\n", elapsed.Round(time.Millisecond))
		if overwrite {
			fmt.Printf("输出方式: 已覆盖原文件\n")
		} else {
			fmt.Printf("输出目录: %s\n", absOut)
		}

		if len(failList) > 0 {
			fmt.Println()
			fmt.Println("失败列表:")
			for _, f := range failList {
				fmt.Printf("  - %s\n", f)
			}
		}
	} else {
		fmt.Printf("完成: %d/%d 成功, %s -> %s (%.1f%%), 耗时 %s\n",
			successFiles, totalFiles,
			formatSize(totalOrig), formatSize(totalComp),
			totalRatio, elapsed.Round(time.Millisecond))
	}

	return failFiles
}

// 带值的命令行选项（后一个token是其值）
var valueFlags = map[string]bool{
	"i": true, "o": true, "dir": true, "out": true,
	"level": true, "j": true, "gs": true, "gsThreads": true, "chunk": true,
}

// reorderArgs 把以"-"开头的选项及其值移动到位置参数之前。
// Go标准库flag遇到第一个位置参数就停止解析，重排后可支持:
//
//	pdfcompress 文件.pdf -level screen
//	pdfcompress 文件.pdf screen（screen作为裸级别另由bareLevel识别）
func reorderArgs(args []string) []string {
	var flags, positionals []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if len(a) > 1 && a[0] == '-' {
			name := strings.TrimLeft(a, "-")
			base := name
			if idx := strings.Index(name, "="); idx >= 0 {
				base = name[:idx] // -level=screen 形式
			}
			flags = append(flags, a)
			// 带值选项：把后一个token一并归为选项部分
			if valueFlags[base] && !strings.Contains(name, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			positionals = append(positionals, a)
		}
	}
	return append([]string{args[0]}, append(flags, positionals...)...)
}

// bareLevel 识别位置参数中的压缩级别关键字或自定义 DPI 数值
// 支持: pdfcompress 文件.pdf screen / pdfcompress 文件.pdf 100
func bareLevel(s string) (CompressLevel, bool) {
	key := strings.ToLower(strings.TrimSpace(s))
	switch key {
	case "screen":
		return LevelScreen, true
	case "ebook":
		return LevelEBook, true
	case "printer":
		return LevelPrinter, true
	case "prepress":
		return LevelPrepress, true
	}
	// 自定义 DPI 数值
	if dpi, err := strconv.Atoi(s); err == nil && dpi >= 10 && dpi <= 2400 {
		return CompressLevel(customLevelBase + dpi), true
	}
	return 0, false
}

func main() {
	inputFile := flag.String("i", "", "输入PDF文件路径")
	outputFile := flag.String("o", "", "输出PDF文件路径（单文件模式，与 -overwrite 互斥）")
	inputDir := flag.String("dir", "", "输入目录（批量模式）")
	outputDir := flag.String("out", "", "输出目录（批量模式，默认在输入目录下创建compressed子目录）")
	recursive := flag.Bool("r", false, "递归处理子目录")
	level := flag.String("level", "ebook", "压缩级别: screen(72DPI), ebook(150DPI), printer(300DPI), prepress(300DPI+), 或自定义数值如 100")
	workers := flag.Int("j", defaultWorkers(), "并发工作数（默认=CPU核数，上限16）")
	gsThreads := flag.Int("gsThreads", 0, "每个Ghostscript进程的渲染线程数（0=按并发数自动）")
	persist := flag.Bool("persist", true, "批量模式复用单个Ghostscript进程处理多个文件（显著提速）")
	chunk := flag.Int("chunk", 0, "单进程处理的文件数（0=自动）")
	overwrite := flag.Bool("overwrite", false, "压缩结果覆盖原文件（先写临时文件，成功后替换；未变小时保留原文件）")
	showVersion := flag.Bool("version", false, "显示版本信息")
	quiet := flag.Bool("q", false, "静默模式")
	gsPath := flag.String("gs", "", "指定Ghostscript路径")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "PDFCompress v%s - PDF批量压缩工具\n", version)
		if runtime.GOOS == "windows" {
			fmt.Fprintf(os.Stderr, "适用于 Windows 7+ 离线环境（内置Ghostscript，无需安装）\n\n")
		} else {
			fmt.Fprintf(os.Stderr, "适用于 麒麟Kylin/统信UOS 等 Linux 系统（随包内置便携Ghostscript，无需安装）\n\n")
		}
		fmt.Fprintf(os.Stderr, "用法:\n")
		fmt.Fprintf(os.Stderr, "  拖放:    把PDF文件或文件夹拖到 pdfcompress 图标上即可（默认ebook级别）\n")
		fmt.Fprintf(os.Stderr, "           支持多选：可一次拖多个PDF/文件夹，逐个处理；目录自动递归扫描\n")
		fmt.Fprintf(os.Stderr, "           输出文件名 = 原名_压缩级别.pdf（如 报告_ebook.pdf）\n")
		fmt.Fprintf(os.Stderr, "  简写:    pdfcompress 文件.pdf [screen|ebook|printer|prepress]\n")
		fmt.Fprintf(os.Stderr, "  单文件:  pdfcompress -i input.pdf -o output.pdf [选项]\n")
		fmt.Fprintf(os.Stderr, "  批量:    pdfcompress -dir <目录> [选项]\n")
		fmt.Fprintf(os.Stderr, "  覆盖:    pdfcompress -dir <目录> -overwrite   （直接替换原文件）\n\n")
		fmt.Fprintf(os.Stderr, "选项:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n压缩级别说明:\n")
		fmt.Fprintf(os.Stderr, "  screen   - 72 DPI，体积最小；中文小字会发糊，不建议用于公文\n")
		fmt.Fprintf(os.Stderr, "  ebook    - 150 DPI，屏幕阅读推荐（默认，速度与清晰度最均衡）\n")
		fmt.Fprintf(os.Stderr, "  printer  - 300 DPI，打印质量（清晰，体积较大）\n")
		fmt.Fprintf(os.Stderr, "  prepress - 300 DPI+，出版级（彩色/灰度内容与 printer 相同，仅黑白位图更精细）\n")
		fmt.Fprintf(os.Stderr, "\n关于不同PDF的压缩效果:\n")
		fmt.Fprintf(os.Stderr, "  文字型（电子公文等）：体积主要来自内嵌字体，压缩靠字体子集化，\n")
		fmt.Fprintf(os.Stderr, "                        四个级别结果基本相同（常见 15MB 压到 200KB 上下）。\n")
		fmt.Fprintf(os.Stderr, "  图片型（扫描件等）  ：体积主要来自扫描图像，级别越高保留分辨率越高，\n")
		fmt.Fprintf(os.Stderr, "                        体积与清晰度差别明显，请按用途选择。\n")
		fmt.Fprintf(os.Stderr, "\n%s\n", author)
	}

	// 重排参数：选项可出现在位置参数前后任意位置（如 文件.pdf -level screen）
	os.Args = reorderArgs(os.Args)
	flag.Parse()

	if *showVersion {
		fmt.Printf("PDFCompress v%s\n", version)
		fmt.Printf("%s\n", author)
		fmt.Printf("Go %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}

	// 双击运行（无任何参数）：显示帮助信息并暂停，不自动退出
	if len(os.Args) == 1 {
		flag.Usage()
		pause()
		return
	}

	if *overwrite && *outputFile != "" {
		fmt.Fprintln(os.Stderr, "错误: -overwrite 与 -o 不能同时使用")
		exitAfterPause(1)
	}

	// 从位置参数中提取裸级别关键字（如: pdfcompress 文件.pdf screen），
	// 其余位置参数视为拖放的文件/目录
	var dropPaths []string
	for _, a := range flag.Args() {
		if _, isLevel := bareLevel(a); !isLevel {
			dropPaths = append(dropPaths, a)
		}
	}

	// 查找Ghostscript
	var gsExe string
	var err error
	if *gsPath != "" {
		gsExe = *gsPath
		if _, err := os.Stat(gsExe); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "错误: 指定的Ghostscript不存在: %s\n", gsExe)
			exitAfterPause(1)
		}
	} else {
		gsExe, err = findGhostscript()
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			fmt.Fprintf(os.Stderr, "\n下载地址: https://github.com/ArtifexSoftware/ghostpdl-downloads/releases\n")
			exitAfterPause(1)
		}
	}

	compressLevel := parseLevel(*level)
	for _, a := range flag.Args() {
		if lvl, ok := bareLevel(a); ok {
			compressLevel = lvl
		}
	}

	// Ghostscript 内部渲染线程数：命令行指定优先，否则自适应
	gsT := *gsThreads
	if gsT <= 0 {
		gsT = calcGSThreads(*workers)
	}
	gsTSingle := *gsThreads
	if gsTSingle <= 0 {
		gsTSingle = capThreads(runtime.NumCPU())
	}

	// 检测 gs 版本：--permit-file-* 需要 gs >= 9.50（9.50 起 SAFER 默认开启）
	usePermits := false
	if *persist {
		if maj, min, ok := detectGSVersion(gsExe); ok {
			usePermits = maj > 9 || (maj == 9 && min >= 50)
		}
	}

	// 拖放/简写模式：位置参数为PDF文件或文件夹（可多选）
	if len(dropPaths) > 0 {
		fmt.Printf("PDFCompress v%s\n", version)
		fmt.Printf("压缩级别: %s\n", levelName(compressLevel))
		fmt.Println(strings.Repeat("=", 60))

		var failTotal int32
		for _, p := range dropPaths {
			info, statErr := os.Stat(p)
			if statErr != nil {
				fmt.Fprintf(os.Stderr, "\n[跳过] 路径不存在: %s\n", p)
				failTotal++
				continue
			}
			if info.IsDir() {
				fmt.Printf("\n>>> 处理文件夹: %s\n", p)
				fails := runBatch(gsExe, p, "", true, compressLevel, *workers, *quiet, gsT,
					*persist, usePermits, *chunk, *overwrite)
				if fails < 0 {
					failTotal++
				} else {
					failTotal += fails
				}
			} else if strings.EqualFold(filepath.Ext(p), ".pdf") {
				fmt.Printf("\n>>> 处理文件: %s\n", p)
				outPath := p
				if !*overwrite {
					ext := filepath.Ext(p)
					outPath = strings.TrimSuffix(p, ext) + "_" + levelSlug(compressLevel) + ext
				}
				if !runSingle(gsExe, p, outPath, compressLevel, *overwrite, *quiet, gsTSingle) {
					failTotal++
				}
			} else {
				fmt.Fprintf(os.Stderr, "\n[跳过] 不是PDF文件: %s\n", p)
			}
		}
		if failTotal > 0 {
			exitAfterPause(1)
		}
		pause()
		return
	}

	if *inputFile == "" && *inputDir == "" {
		flag.Usage()
		fmt.Fprintln(os.Stderr, "\n错误: 请指定输入文件(-i)或输入目录(-dir)，或直接把PDF/文件夹拖到程序图标上")
		exitAfterPause(1)
	}
	if *inputFile != "" && *inputDir != "" {
		flag.Usage()
		fmt.Fprintln(os.Stderr, "\n错误: -i 和 -dir 不能同时使用")
		exitAfterPause(1)
	}

	if !*quiet {
		fmt.Printf("PDFCompress v%s\n", version)
		fmt.Printf("Ghostscript: %s\n", gsExe)
		fmt.Printf("压缩级别: %s\n", levelName(compressLevel))
		if *inputFile != "" {
			fmt.Printf("gs渲染线程: %d\n", gsTSingle)
		}
		fmt.Println()
	}

	// 单文件模式
	if *inputFile != "" {
		if _, err := os.Stat(*inputFile); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "错误: 文件不存在: %s\n", *inputFile)
			exitAfterPause(1)
		}
		outPath := *outputFile
		if *overwrite {
			outPath = *inputFile
		} else if outPath == "" {
			ext := filepath.Ext(*inputFile)
			base := strings.TrimSuffix(*inputFile, ext)
			outPath = base + "_" + levelSlug(compressLevel) + ext
		}
		if !*quiet {
			fmt.Printf("输入文件: %s\n", *inputFile)
			if *overwrite {
				fmt.Printf("输出方式: 覆盖原文件\n")
			} else {
				fmt.Printf("输出文件: %s\n", outPath)
			}
			fmt.Println("正在压缩...")
		}
		if !runSingle(gsExe, *inputFile, outPath, compressLevel, *overwrite, *quiet, gsTSingle) {
			exitAfterPause(1)
		}
		return
	}

	// 批量模式
	if *inputDir != "" {
		dirInfo, err := os.Stat(*inputDir)
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "错误: 目录不存在: %s\n", *inputDir)
			exitAfterPause(1)
		}
		if !dirInfo.IsDir() {
			fmt.Fprintf(os.Stderr, "错误: 路径不是目录: %s\n", *inputDir)
			exitAfterPause(1)
		}
		if failFiles := runBatch(gsExe, *inputDir, *outputDir, *recursive, compressLevel,
			*workers, *quiet, gsT, *persist, usePermits, *chunk, *overwrite); failFiles != 0 {
			exitAfterPause(1)
		}
	}
}
