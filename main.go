package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	version = "2.4.0"
	author  = "https://github.com/playgitboy"
)

// ---- 卡死保护（停滞超时）----
//
// 判据不是「总耗时」，而是「两次输出之间的间隔」。
//
// 正常的大文件（例如 1GB 扫描件）跑几分钟完全正常，用固定总时长做超时必然
// 误杀它；而真正卡死的特征是「进程活着但长时间一声不吭」。Ghostscript 每处理
// 完一页就会往 stdout 打印一行 "Page N"，这行进度就是「还在推进」的证据。
//
// 因此这里只在「连续 stallTimeout 时长内没有任何输出」时才判定卡死并终止进程，
// 只要还在持续输出，跑多久都不会被打断。
var (
	// stallTimeout 停滞阈值：连续这么久没有任何输出即判定卡死。0 = 关闭该保护。
	stallTimeout = 180 * time.Second

	// firstOutputTimeout 首次输出阈值：进程启动后这么久仍无任何输出即判定卡死。
	// 用于覆盖「gs 根本起不来」的情形（正常情况下约 300ms 就会打印第一行）。
	firstOutputTimeout = 60 * time.Second
)

const (
	// watchdogInterval 看门狗检查间隔
	watchdogInterval = time.Second

	// maxGSOutputBytes 采集 gs 输出的上限。
	// 正常失败场景输出只有几 KB；进度输出每页约 8 字节，4MB 足够容纳约 50 万页，
	// 因此该上限实际不会被触发，仅作为内存兜底。
	maxGSOutputBytes = 4 << 20
)

// errGSStalled 表示 gs 因长时间无输出被判为卡死（区别于普通的 gs 报错）。
var errGSStalled = errors.New("处理停滞")

// tableSepWidth 结果表格分隔线的宽度。
//
// Windows 7 的命令提示符窗口默认 80 列，且属性里最大宽度也受限（不像新系统
// 可以随意拖宽），分隔线一旦超过窗口宽度就会被折行，看起来像多出一条横线，
// 影响观感。原为 100 列，减 23 列后取 77，可完整容纳于 80 列窗口。
const tableSepWidth = 77

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

// validLevel 判断 -level 取值是否能被识别（内置档位名或 10-2400 的自定义 DPI）。
// 用于对无效输入给出提示，而不是静默回退到 ebook —— 静默回退会让用户
// 以为指定生效了，实际档位与预期不符。
func validLevel(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "screen", "ebook", "printer", "prepress":
		return true
	}
	dpi, err := strconv.Atoi(s)
	return err == nil && dpi >= 10 && dpi <= 2400
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
		// ⚠ 刻意不使用 -dQUIET。
		//
		// -dQUIET 会把 gs 的进度输出（"Processing pages N through M."、"Page N"）
		// 一并抑制（实测该参数下 stdout/stderr 全程为 0 字节），而「卡死保护」
		// 正是靠这些进度行判断进程是否还在推进 —— 静默之后就没有任何信号可用了。
		//
		// 保持非静默的代价只是输出缓冲里多出若干行进度文本（每页约 8 字节，
		// 且已在 runGS 中做了 4MB 上限兜底），对速度与压缩结果均无影响。
		// 错误与警告本就不受 -dQUIET 抑制，因此去掉它不会改变异常文件的处理结果。
		"-dBATCH",
		"-dAutoRotatePages=/None",
		// ⚠ 关闭 JPEG 透传是「压缩率≈0」的关键修复之一。
		// 透传开启时，源图是 JPEG 且 gs 认为“无需重采样”时会把原图原样搬进输出，
		// 档位形同虚设 —— 实测 200DPI 的手机/扫描仪 PDF，在 ebook(150) 档
		// 压缩率恰好为 0%（输出比原文件还大几百字节，被安全网判为 KEEP）。
		// 关闭后强制重新编码，配合下面的 DownsampleThreshold=1.0，
		// 让真正超标的图像被降到目标分辨率，净收益远大于重编码开销。
		"-dPassThroughJPEGImages=false",
		"-dPassThroughJPXImages=false",
		"-dDetectDuplicateImages=true",
		"-dEmbedAllFonts=true", // 文字型PDF（内嵌大字体）压缩主力：子集化+嵌入
		"-dCompressFonts=true",
		"-dSubsetFonts=true",
		"-dDownsampleColorImages=true",
		"-dDownsampleGrayImages=true",
		"-dDownsampleMonoImages=true",
		// ⚠ DownsampleThreshold 默认 1.5，含义是「源分辨率 > 目标×1.5 才降采样」。
		// ebook(150DPI) 遇到 200DPI 扫描件时 150×1.5=225 > 200 → 不降采样 →
		// 图像一个像素都没少，压缩率自然接近 0。
		// 设为 1.0：只要高于目标分辨率就降采样，使「档位 DPI」成为真正的上限，
		// 与档位名称（screen 72 / ebook 150 / printer 300）的语义一致。
		"-dColorImageDownsampleThreshold=1.0",
		"-dGrayImageDownsampleThreshold=1.0",
		"-dMonoImageDownsampleThreshold=1.0",
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
//	"std"                   —— 标准纸张或无法判定：沿用原有参数，"行为与旧版完全一致"
//	"fix:A4:P:631.5x842.0" 等 —— 像素当 pt 扫描件：启用 FIXEDMEDIA/FitPage/-r 修正
//
// ⚠ 键里必须带上「目标页面尺寸」，不能只带纸张名。
// 原因：目标尺寸由 targetPageSize 按**原始长宽比**反算（见该函数注释），
// 所以同为 A4 的两份扫描件，若原稿比例不同（如 4:3 手机拍摄 vs √2 扫描仪），
// 目标尺寸并不相同。若只按纸张名分组，它们会被塞进同一进程、被迫共用
// chunk[0] 的页面尺寸 —— 其中一份会被加上多余白边，且压缩率随「邻居是谁」漂移。
// 带上尺寸后，任一文件的处理参数只由自身决定，与批次组成无关，
// 也与「单文件模式」的结果完全一致。
//
// ⚠ 键里还必须带上方向（P=竖版 / L=横版）。
// targetPageSize 返回的是竖版视角的 (短边, 长边)；横版文件在 geometryArgs 中
// 会把宽高对调。若不把方向写进分组键，竖版与横版文件会被归入同一进程，
// 而设备宽高是进程级参数，必然有一方被套错方向、内容被缩放留白。
func geomGroupKey(g pdfGeom) string {
	if !g.shouldFixGeometry() {
		return "std"
	}
	name := g.paper.name
	if name == "" {
		name = "A4"
	}
	orient := "P"
	if g.landscape {
		orient = "L"
	}
	pw, ph := targetPageSize(g)
	return fmt.Sprintf("fix:%s:%s:%.1fx%.1f", name, orient, pw, ph)
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
//     （该参数现已并入 baseGSArgs 全局生效，此处保留以表明本场景的强依赖关系。）
//
// 若该文件不需要修正（标准纸张/无图像/无法判定），返回 nil，调用方行为完全不变。
func geometryArgs(g pdfGeom, level CompressLevel) []string {
	if !g.shouldFixGeometry() {
		return nil
	}
	dpi := levelDPI(level)
	pw, ph := targetPageSize(g)
	// 保持原始页面方向：targetPageSize 返回竖版视角的 (短边, 长边)，
	// 横版文件需对调，否则横版内容会被压进竖版页面、上下留大片白边。
	if g.landscape {
		pw, ph = ph, pw
	}
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

// samePath 判断两个路径是否指向同一个文件/目录。
// 优先用 os.Stat + os.SameFile（可识别符号链接、相对/绝对、硬链接、
// Windows 大小写差异）；目标尚不存在时退回绝对路径比较。
func samePath(a, b string) bool {
	if ai, aerr := os.Stat(a); aerr == nil {
		if bi, berr := os.Stat(b); berr == nil {
			return os.SameFile(ai, bi)
		}
	}
	aa, aerr := filepath.Abs(a)
	bb, berr := filepath.Abs(b)
	if aerr != nil || berr != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	aa, bb = filepath.Clean(aa), filepath.Clean(bb)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(aa, bb) // Windows 文件系统不区分大小写
	}
	return aa == bb
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
	msg := gsErrorLine(out)
	if msg == "" {
		return err
	}
	return fmt.Errorf("%v: %s", err, msg)
}

// gsErrorLine 从 gs 的输出里挑出最有信息量的一行作为错误说明。
//
// 去掉 -dQUIET 之后，gs 会先打印版本横幅（"GPL Ghostscript 9.56.1 ..."），
// 若沿用原来的「取第一行」策略，错误说明就会变成版权信息。
// 因此这里优先查找形如 "**** Error: ..." 的硬错误行，找不到才退回第一行。
func gsErrorLine(out string) string {
	first := ""
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if first == "" {
			first = ln
		}
		if isGSFaultLine(ln) {
			return clipMsg(ln)
		}
	}
	return clipMsg(first)
}

// isGSFaultLine 判断某行是否为 Ghostscript 的硬错误行（而非进度、横幅或警告汇总）
func isGSFaultLine(ln string) bool {
	return strings.Contains(ln, "Error:") ||
		strings.Contains(ln, "Unrecoverable error") ||
		strings.Contains(ln, "Couldn't") ||
		strings.Contains(ln, "No pages will be processed") ||
		strings.Contains(ln, "requires a password")
}

// clipMsg 把单行消息按字节截断到 200 字符
func clipMsg(msg string) string {
	if len(msg) > 200 {
		return msg[:200] + "..."
	}
	return msg
}

// runGS 运行 Ghostscript，并在「长时间没有任何输出」时判定卡死、终止进程。
//
// 判据不是「总耗时」而是「两次输出之间的间隔」：正常大文件跑几分钟很常见，
// 但只要它还在持续打印页进度（"Page N"），就说明在处理，绝不能打断；
// 真正卡死的特征是「进程活着却长时间一声不吭」。因此只有连续 stallTimeout
// 内毫无输出时才终止，这样 1GB 级别的大文件永远不会被误杀。
//
// 返回 gs 合并后的输出（stdout + stderr）与错误。
// 因停滞被终止时，返回的错误包装了 errGSStalled，调用方可用 errors.Is 识别。
func runGS(gsExe string, args []string) (string, error) {
	cmd := exec.Command(gsExe, args...)
	cmd.Stdin = nil

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return "", err
	}

	var mu sync.Mutex
	var buf strings.Builder
	var lastNano int64 // 0 表示「尚未收到任何输出」

	// 并发读取两路管道：只要读到任意字节就刷新时间戳，作为「仍在推进」的证据。
	// 不按行读取 —— 这样即使 gs 用 \r 刷新进度也能被及时感知。
	var rwg sync.WaitGroup
	consume := func(r io.Reader) {
		defer rwg.Done()
		b := make([]byte, 32*1024)
		for {
			n, rerr := r.Read(b)
			if n > 0 {
				atomic.StoreInt64(&lastNano, time.Now().UnixNano())
				mu.Lock()
				if buf.Len() < maxGSOutputBytes {
					buf.Write(b[:n])
				}
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}
	rwg.Add(2)
	go consume(stdout)
	go consume(stderr)

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	var runErr error
	stalled := false
	var stalledLimit time.Duration

	if stallTimeout > 0 {
		ticker := time.NewTicker(watchdogInterval)
	watchdog:
		for {
			select {
			case runErr = <-waitErr:
				break watchdog
			case <-ticker.C:
				n := atomic.LoadInt64(&lastNano)
				limit := stallTimeout
				var idle time.Duration
				if n == 0 {
					// 还没有任何输出：用更短的「首次输出」阈值兜底
					limit = firstOutputTimeout
					idle = time.Since(start)
				} else {
					idle = time.Since(time.Unix(0, n))
				}
				if idle > limit {
					stalled = true
					stalledLimit = limit
					_ = cmd.Process.Kill()
					// 等待回收；极端情况下 Wait 可能迟迟不返回，加超时兜底避免主流程挂死
					select {
					case runErr = <-waitErr:
					case <-time.After(10 * time.Second):
					}
					break watchdog
				}
			}
		}
		ticker.Stop()
	} else {
		runErr = <-waitErr
	}

	// 等读取协程收尾（进程退出后管道关闭会自然返回）；同样加超时兜底
	rwgDone := make(chan struct{})
	go func() { rwg.Wait(); close(rwgDone) }()
	select {
	case <-rwgDone:
	case <-time.After(5 * time.Second):
	}

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	if stalled {
		return out, fmt.Errorf("%w：连续 %s 没有任何进度输出，已终止（已保留原文件）",
			errGSStalled, stalledLimit.Round(time.Second))
	}
	return out, runErr
}

// gsFatalReadProblem 识别「源文件根本没能被 gs 读入」的致命情形。
//
// 背景（2026-10-15 实测）：对加密 PDF、结构损坏的 PDF，gs 依然会以**退出码 0**
// 结束，并把问题只写在 stderr 里；同时它仍会写出一个仅含空白页的极小 PDF
// （约 2-3 KB）。由于体积远小于原文件，仅凭体积的安全网会把这种退化输出
// 误判为「压缩成功」，甚至在 --overwrite 模式下用空白文件替换掉原文件。
// 因此必须显式识别这些签名，直接判为失败并保留原文件。
//
// 返回面向用户的错误说明；未命中任何致命签名时返回 nil。
func gsFatalReadProblem(out string) error {
	switch {
	case strings.Contains(out, "requires a password for access"):
		return fmt.Errorf("源文件已加密，需要密码才能压缩（已保留原文件）")
	case strings.Contains(out, "Couldn't initialise file"):
		return fmt.Errorf("无法解析源文件（可能已加密或结构损坏，已保留原文件）")
	case strings.Contains(out, "No pages will be processed"):
		return fmt.Errorf("源文件未解析出任何页面（已保留原文件）")
	case strings.Contains(out, "Unrecoverable error"):
		return fmt.Errorf("源文件无法解析（已保留原文件）")
	}
	return nil
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
//
// 带超时：版本探测是启动路径上的第一件事，万一 gs 无法正常响应 --version，
// 不能让整个流程无限期挂住。10 秒足够任何正常的 gs 返回。
func detectGSVersion(gsExe string) (major, minor int, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, gsExe, "--version").Output()
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

	out, runErr := runGS(gsExe, args)
	// 停滞超时：进程是被我们主动终止的，产物必然残缺，
	// 直接判失败并清掉半成品（覆盖模式下原文件不会被动到）。
	if errors.Is(runErr, errGSStalled) {
		os.Remove(output)
		return runErr
	}
	if runErr != nil {
		return gsErr(runErr, out)
	}
	// 源文件没能被读入时 gs 仍返回 0，却写出一个空白迷你 PDF；
	// 这里必须显式判失败，否则会把它当成「压缩成功」。
	if msg := gsFatalReadProblem(out); msg != nil {
		os.Remove(output)
		return msg
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

	out, runErr := runGS(gsExe, args)
	// 停滞超时：整块进程被终止，返回错误让上层降级为逐文件处理，
	// 由 compressFile 逐个定位并单独为卡死的那个文件计时。
	if errors.Is(runErr, errGSStalled) {
		return runErr
	}
	if runErr != nil {
		return gsErr(runErr, out)
	}
	// 块内只要有任何文件是「加密/无法解析」，就在共享输出里留下致命签名。
	// 此时整块结果不可信：返回错误让上层降级为逐文件处理，由 compressFile
	// 逐个精确定位并为坏文件保留原文。正常文件不会命中这些签名。
	if msg := gsFatalReadProblem(out); msg != nil {
		return msg
	}
	return nil
}

// 查找所有PDF文件
// skip 为需要跳过的目录集合（输出目录），避免二次运行时把上一次的输出当成输入。
// 优化：不再对每个条目调用 filepath.Abs（其内部会触发 os.Getwd 系统调用），
// 改为比较 Clean 后的路径，避免 O(N) 次系统调用。
func findPDFs(root string, recursive bool, skip map[string]bool) ([]string, []string, error) {
	var pdfs []string
	var unreadable []string

	if recursive {
		cleanRoot := filepath.Clean(root)
		err := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
			if werr != nil {
				// 根目录本身不可读：必须报错（否则会静默退化为「未找到PDF」）
				if filepath.Clean(path) == cleanRoot {
					return werr
				}
				// 单个子目录/文件不可读（权限、临时占用、坏符号链接等）：
				// 记录并跳过，不能因为一个坏目录就放弃整批文件。
				unreadable = append(unreadable, path)
				return nil
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
			return nil, unreadable, err
		}
	} else {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".pdf") {
				pdfs = append(pdfs, filepath.Join(root, e.Name()))
			}
		}
	}

	sort.Strings(pdfs) // 固定顺序，便于结果对照与复现
	return pdfs, unreadable, nil
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
	// 非覆盖模式下输出路径不能指向输入文件本身：否则 gs 会把结果写回它正在读入的
	// 源文件，静默丢失原文件（实测 47KB 输入被 3KB 输出直接替换）。
	// 这属于必须拦截的数据丢失路径。
	if !overwrite && samePath(input, output) {
		fmt.Fprintf(os.Stderr, "错误: 输出文件与输入文件相同（%s）\n", output)
		fmt.Fprintln(os.Stderr, "      非覆盖模式会直接改写源文件；请指定不同的 -o 路径，或改用 -overwrite。")
		return false
	}

	start := time.Now()
	origSize := fileSize(input)

	writePath := output
	if overwrite {
		writePath = tempOutputPath(filepath.Dir(input), 0)
	}

	if err := compressFile(gsExe, input, writePath, level, gsThreads); err != nil {
		// 失败时清掉本次可能写出的半成品（含 gs 产出的空白迷你 PDF），
		// 避免在输出目录留下误导性的文件。
		os.Remove(writePath)
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

	// 非覆盖模式下，输出目录 == 输入目录是一条数据丢失路径：
	// gs 会被要求把结果写回它正在读入的源文件。实测小文件被静默替换
	// （原文件直接丢失），大文件则可能在读到一半时被截断而损坏。
	// 必须在扫描前拦下，绝不静默改写源文件。
	if !overwrite && samePath(absOut, absDir) {
		fmt.Fprintf(os.Stderr, "错误: 输出目录与输入目录相同（%s）\n", absOut)
		fmt.Fprintln(os.Stderr, "      非覆盖模式会直接改写源文件；请用 -out 指定不同的输出目录，")
		fmt.Fprintln(os.Stderr, "      或改用 -overwrite 明确表示要以压缩结果覆盖原文件。")
		return -1
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

	pdfs, unreadable, err := findPDFs(absDir, recursive, skipDirs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "扫描文件失败: %v\n", err)
		return -1
	}
	// 个别子目录/文件不可读时只跳过并提示，不让整批任务失败。
	if len(unreadable) > 0 {
		fmt.Fprintf(os.Stderr, "[提示] 有 %d 个目录/文件无法读取，已跳过：\n", len(unreadable))
		for _, u := range unreadable {
			fmt.Fprintf(os.Stderr, "  - %s\n", truncName(u, 80))
		}
		fmt.Fprintln(os.Stderr)
	}
	if len(pdfs) == 0 {
		fmt.Println("未找到PDF文件")
		return 0
	}

	// 几何探测：并行完成（每个文件最多读 8MiB）。
	// 大目录下探测是纯 IO 等待，串行会造成明显的前置延迟；并行度取较小值（4），
	// 兼顾机械盘/网络盘上的顺序读性能与内存占用（每路峰值约 8MiB）。
	geoms := make([]pdfGeom, len(pdfs))
	probeWorkers := workers
	if probeWorkers > 4 {
		probeWorkers = 4
	}
	if probeWorkers > len(pdfs) {
		probeWorkers = len(pdfs)
	}
	if probeWorkers < 1 {
		probeWorkers = 1
	}
	var probeIdx int32
	var probeWg sync.WaitGroup
	for pw := 0; pw < probeWorkers; pw++ {
		probeWg.Add(1)
		go func() {
			defer probeWg.Done()
			for {
				i := int(atomic.AddInt32(&probeIdx, 1)) - 1
				if i >= len(pdfs) {
					return
				}
				geoms[i] = probePDFGeom(pdfs[i])
			}
		}()
	}
	probeWg.Wait()

	// 构造任务并预建输出目录（原实现每个文件都 MkdirAll 一次）
	tasks := make([]*fileTask, 0, len(pdfs))
	dirCache := make(map[string]bool, 16)
	var geomDegraded []string
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
		t.geom = geoms[i]
		t.geomKey = geomGroupKey(t.geom)
		if t.geom.readDegraded {
			geomDegraded = append(geomDegraded, rel)
		}
		tasks = append(tasks, t)
	}

	// 探测降级提示：读取不完整的文件，其几何判定可能失真 ——
	// 本该启用修正的扫描件会被按常规参数处理，压缩率低于预期且无其它迹象。
	// 如实提示（走 stderr，不干扰 stdout 的结果行），而不是静默降质。
	if len(geomDegraded) > 0 {
		fmt.Fprintf(os.Stderr,
			"[提示] %d 个文件读取不完整，几何判定可能失真（将按常规参数处理，压缩率或低于预期）：\n",
			len(geomDegraded))
		for _, n := range geomDegraded {
			fmt.Fprintf(os.Stderr, "  - %s\n", truncName(n, 60))
		}
		fmt.Fprintln(os.Stderr)
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
		fmt.Println(strings.Repeat("-", tableSepWidth))
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

	// 进度提示：单进程批量模式下，一个块内的多个文件要等整个块跑完才会打印结果，
	// 大文件场景下会「长时间没有任何输出」，用户容易误以为卡死。
	// 因此块一开始就打印一行“开始处理”，让用户始终能看到进展。
	var progressMu sync.Mutex
	var startedChunks int32

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

				// 块内多文件时先报进度，避免「长时间无输出」被误判为卡死
				if !quiet && len(chunk) > 1 {
					n := atomic.AddInt32(&startedChunks, 1)
					progressMu.Lock()
					fmt.Printf("  ... 开始第 %d/%d 批（%d 个文件，起始：%s）\n",
						n, len(chunks), len(chunk), truncName(chunk[0].rel, 40))
					progressMu.Unlock()
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
		fmt.Println(strings.Repeat("-", tableSepWidth))
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
	"timeout": true,
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
	timeoutSec := flag.Int("timeout", 180,
		"卡死保护：连续该秒数无任何进度输出即判定卡死并跳过该文件（0=关闭；大文件处理慢不受影响，只要仍在输出进度就不会触发）")

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
		fmt.Fprintf(os.Stderr, "\n关于大文件与卡死保护:\n")
		fmt.Fprintf(os.Stderr, "  大文件处理慢是正常的（1GB 扫描件处理几分钟很常见），只要 Ghostscript\n")
		fmt.Fprintf(os.Stderr, "  仍在输出页进度就不会被打断；只有连续无任何输出达到 -timeout 秒数时才\n")
		fmt.Fprintf(os.Stderr, "  判定为卡死，跳过该文件并保留原文件（不影响其他文件）。\n")
		fmt.Fprintf(os.Stderr, "  个别重度损坏的文件会让 gs 长时间无输出，该保护可避免整批任务被拖住。\n")
		fmt.Fprintf(os.Stderr, "\n%s\n", author)
	}

	// 重排参数：选项可出现在位置参数前后任意位置（如 文件.pdf -level screen）
	os.Args = reorderArgs(os.Args)
	flag.Parse()

	// 停滞超时配置（0 = 关闭卡死保护）。
	// 首次输出阈值正常为 60s，避免用户设了很小的 -timeout 时反被它拖慢。
	if *timeoutSec > 0 {
		stallTimeout = time.Duration(*timeoutSec) * time.Second
		firstOutputTimeout = 60 * time.Second
		if firstOutputTimeout > stallTimeout {
			firstOutputTimeout = stallTimeout
		}
	} else {
		stallTimeout = 0
	}

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
	if !validLevel(*level) {
		fmt.Fprintf(os.Stderr, "[警告] 未识别的压缩级别 %q，已回退为默认的 ebook（150 DPI）。\n", *level)
		fmt.Fprintln(os.Stderr, "       可用值: screen / ebook / printer / prepress，或 10-2400 的自定义 DPI 数值。")
	}
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
