package main

// pdfgeom.go —— PDF 页面几何探测与「智能几何基准修正」判定。
//
// 背景（2026-10-15 实测结论）：
//
//	手机拍摄 / 扫描仪导出的 PDF，常见「页面尺寸 = 图片像素」的做法：
//	一张 2736×3648 的照片，被放进一个同样 2736×3648 pt 的页面里。
//	此时 Ghostscript 按页面物理尺寸反推图像有效 DPI：
//	    有效DPI = 2736px / (2736pt / 72) = 72 DPI
//	72 DPI 低于所有档位阈值（screen 72 为临界、ebook 150、printer 300），
//	于是 gs 判定「图像已经足够低分辨率、无需降采样」，
//	四个档位全部走进同一条「不降采样」路径 → 输出字节几乎完全相同、
//	压缩率接近 0。这正是实测中 screen/ebook/printer 毫无差异的根因。
//
// 解决办法：对这类「像素当 pt」的页面，先用 -dFIXEDMEDIA -dPDFFitPage
// 把页面重设为「与之比例最接近的标准纸张」，同时用 -r<档位DPI> 重设基准，
// gs 才能算出真实 DPI 并执行降采样。
//
// 关键设计原则（用户明确要求）：
//  1. 智能条件判断，不搞一刀切 —— 只对「异常页面」启用修正；
//     标准 A4/A3/Letter 等页面（电子公文）必须保持原有行为完全不变。
//  2. 修正用的目标纸张按页面比例选择（A4 或 A3），不强制塞进 A4，
//     避免 A3 图纸被压扁或留白。
//
// 本文件只做「几何探测」，不含任何 gs 调用与副作用。

import (
	"bytes"
	"os"
	"sort"
)

// geomKind 描述页面几何属于哪一类
type geomKind int

const (
	geomStandard  geomKind = iota // 标准纸张（A4/A3/Letter 等）→ 不改动，走原逻辑
	geomPixelAsPt                 // 像素当 pt 的扫描件 → 需要几何基准修正
	geomUnknown                   // 无法判定（解析失败/无 MediaBox）→ 不改动，走原逻辑
)

// paperSpec 标准纸张定义（单位 pt，竖版）
type paperSpec struct {
	name string
	w    float64
	h    float64
}

// 常见标准纸张（ISO 216 A 系列 + 北美 Letter/Legal）。
// 判定容差放得较宽，因为不同软件导出的 pt 值会有 ±2pt 的取整差异。
var standardPapers = []paperSpec{
	{"A3", 842, 1191},
	{"A4", 595, 842},
	{"A5", 420, 595},
	{"A6", 298, 420},
	{"Letter", 612, 792},
	{"Legal", 612, 1008},
	{"B4", 709, 1001},
	{"B5", 499, 709},
}

// 判定为「非标准纸张」的页面尺寸下限（pt）。
// 标准纸张最大边不超过 1191pt(A3) / 1008pt(Legal)；一旦最大边远超此值，
// 基本可以确定是「像素当 pt」的扫描件（如 2480×3508、2736×3648）。
const oversizeThresholdPt = 1300.0

// 判定标准纸张时的容差：相对偏差 3% 以内视为同一纸张。
// 实测：A4 导出常见 595.0/595.276/596，均在容差内。
const paperRelTol = 0.03

// pageGeom 单页几何探测结果
type pageGeom struct {
	kind geomKind
	// 归一化后的页面宽高（始终 w<=h，即竖版视角），单位 pt
	w, h float64
	// 修正时应使用的目标纸张（仅 geomPixelAsPt 有效）
	paper paperSpec
}

// maxDim 返回页面长边
func (g pageGeom) maxDim() float64 {
	if g.w > g.h {
		return g.w
	}
	return g.h
}

// minDim 返回页面短边
func (g pageGeom) minDim() float64 {
	if g.w < g.h {
		return g.w
	}
	return g.h
}

// aspect 返回长边/短边之比（>=1）
func (g pageGeom) aspect() float64 {
	if g.minDim() <= 0 {
		return 0
	}
	return g.maxDim() / g.minDim()
}

// pdfGeom 是整个 PDF 文件的几何探测汇总
type pdfGeom struct {
	kind geomKind
	// 目标纸张（仅当 kind==geomPixelAsPt）
	paper paperSpec
	// 探测到的页面数
	pages int
	// 是否所有页面都是同一尺寸（忽略方向差异，见 probePDFGeom 中的说明）
	uniform bool
	// 是否混排方向：既有竖版又有横版页面。
	// 影响修正策略：gs 的 FIXEDMEDIA 是「整份文件一个页面尺寸」，无法按页切换，
	// 因此混排文件只能按**多数方向**设置目标页面（见 landscape）。
	// 少数方向的页面会被 PDFFitPage 等比缩放后居中（上下或左右留白），
	// 但比例与内容完整，不会拉伸变形。
	mixedOrientation bool
	// landscape：主尺寸页面是否以横版为主（长边作宽）。
	//
	// 修正时必须保持原始方向：若把横版扫描件强行套进竖版纸张，
	// 内容会被等比缩小到页面宽度、上下各留一大片白边（实测横版 A4 内容
	// 在竖版页面上只剩约 70% 线性尺寸）。按原方向设置目标页面后，
	// 横版内容可铺满整个页面，压缩率与观感同时更好。
	landscape bool
	// hasImage：文件中是否含图像对象(XObject/Image 或内联图像)。
	// 用途：几何基准修正的唯一收益是「让 gs 重新降采样图像」。
	// 对纯文字 PDF（无图像）该修正毫无收益，反而会因页面结构改写导致
	// 体积略微变大（实测纯文字扫描件样例 2.4KB→5.1KB）。
	// 因此只对「含图像」的文件启用修正。
	hasImage bool
	// 页面宽高的原始值样本（用于调试/日志）
	sampleW, sampleH float64
	// readDegraded：探测时对文件头/尾窗口的读取未能完整完成
	// （短读或 Seek 失败，重试后仍不完整）。
	//
	// 为什么必须记下来：本函数是「尽力而为」——读取不全时解析会静默退化，
	// 本该启用几何修正的「像素当 pt」扫描件会被归入 std 分组，
	// 于是压缩率骤降且没有任何迹象。调用方据此给出行提示，
	// 而不是把「探测不到」误当成「无需修正」。
	readDegraded bool
}

// 判定是否需要对该 PDF 启用「几何基准修正」
func (g pdfGeom) shouldFixGeometry() bool {
	return g.kind == geomPixelAsPt && g.hasImage
}

// targetPageSize 计算几何基准修正时应写入 gs 的目标页面尺寸（pt）。
//
// 设计要点：**保持原始长宽比**。
//
// 单纯把扫描件套进 A4 会引入比例偏差，例如：
//
//	4:3 手机拍摄 aspect = 1.3333，A4 aspect = 1.4151 → 拉伸 6.1%
//
// 对扫描件而言，比例本身就携带了「原稿是 A4/A3/B5/相机比例」的信息，
// 强行归一化到某个标准纸张只会让内容区在页面上留下不均匀白边。
//
// 因此做法是：
//  1. 用 scanPaperFor 选出的纸张确定「面积量级」（长边物理尺寸）；
//  2. 保持原始长宽比，反算出对应的宽高。
//
// 效果：4:3 相机扫描件的目标页面仍是 4:3，不拉伸；
// 而 √2 比例的扫描件因为比例本就贴近 A4/A3，结果与标准纸张几乎一致。
func targetPageSize(g pdfGeom) (w, h float64) {
	paper := g.paper
	if paper.name == "" {
		paper = scanPaperCandidates[0]
	}
	// 原始页面（竖版视角）的长宽比
	sw, sh := g.sampleW, g.sampleH
	if sw > sh {
		sw, sh = sh, sw
	}
	aspect := 0.0
	if sw > 0 {
		aspect = sh / sw
	}
	if aspect <= 0 {
		// 兜底：直接用纸张本身尺寸
		return paper.w, paper.h
	}

	// 以纸张的长边为准，按原始比例反算短边。
	// 这样页面的「绝对大小」与标准纸一致（保证 gs 算出的 DPI 正确），
	// 同时比例与原始原稿一致（不留畸形白边）。
	long := paper.h
	short := long / aspect

	// 合理性钳制：短边不应超过长边，也不应小于纸张短边的 70%
	// （避免异常比例导致页面过窄/过宽）
	if short > long {
		short = long
	}
	if short < paper.w*0.7 {
		short = paper.w * 0.7
	}
	if short > paper.w*1.4 {
		short = paper.w * 1.4
	}
	return short, long
}

// paperForAspect 依据页面长宽比，在给定候选列表中挑选最接近的标准纸张。
//
// 仅比较长宽比（而非绝对尺寸），因为扫描件的绝对尺寸本来就是「像素」，
// 比例才是真实信息。
//
// ⚠ 注意：A4 与 A3 的长宽比完全相同（都是 √2≈1.414），ISO 216 系列均如此。
// 因此当候选里同时含 A4/A3 时，本函数无法靠比例区分二者；
// 出现并列时统一取「面积较小」的那张（即 A4），保证结果确定、不随列表顺序漂移。
// 需要按规模区分 A4/A3 的场景请用 scanPaperFor。
func paperForAspect(aspect float64, candidates []paperSpec) paperSpec {
	best := candidates[0]
	bestDiff := 1e18
	for _, p := range candidates {
		pa := p.h / p.w // 竖版长宽比
		d := pa - aspect
		if d < 0 {
			d = -d
		}
		// 严格更贴近才替换；完全并列时保留面积更小者
		if d < bestDiff-1e-9 {
			bestDiff = d
			best = p
		} else if absFloat(d-bestDiff) <= 1e-9 && p.w*p.h < best.w*best.h {
			best = p
		}
	}
	return best
}

// 扫描件归一化时的候选纸张。
// 用户约定：扫描件以 A4 为主、A3 少见。这里把两者都纳入候选，
// 由长宽比自动决定用哪一个 —— A4 与 A3 长宽比相同(√2≈1.414)，
// 因此实际是靠「绝对像素规模」来区分：宽高都超过 A3 的像素范围才用 A3。
var scanPaperCandidates = []paperSpec{
	{"A4", 595, 842},
	{"A3", 842, 1191},
	{"Letter", 612, 792},
	{"Legal", 612, 1008},
}

// 标准纸张的竖版长宽比（用于比例分类）
const (
	aspectLetter = 792.0 / 612.0  // ≈1.2941
	aspectLegal  = 1008.0 / 612.0 // ≈1.6471
)

// 判为「北美纸张」所需的贴近程度（长宽比绝对差）。
//
// 阈值取 0.015 —— 刻意收紧。理由：
// 本工具面向国内政务/企业文档，A4 是绝对主力。
// 常见「不典型比例」与北美纸张的差距其实很小，例如：
//   - 4:3 手机拍摄 1.3333，距 Letter(1.2941) 差 0.039
//   - 16:9 截图     1.7778，距 Legal(1.6471) 差 0.131
//
// 若阈值放宽到 0.05，4:3 照片会被误判为 Letter。
// 收紧到 0.015 后，只有真正的 Letter/Legal 扫描件（比例几乎精确）才会命中，
// 其余一律按 √2 系列处理（A4 / A3），符合用户「扫描件以 A4 为主」的约定。
const naAspectTol = 0.015

// scanPaperFor 为扫描件挑选「基准纸张」。
//
// 注意：这里返回的 paper 只用来确定**目标页面的长边物理尺寸**（即该扫描件
// 「应该有多大」），最终写入 gs 的页面尺寸还会按原始长宽比微调，
// 见 targetPageSize —— 这样可避免 4:3 相机照片被硬套 A4 造成 6% 的比例拉伸。
//
// 规则（按优先级）：
//  1. 若长宽比接近北美系（Letter 1.294 / Legal 1.647），且在 naAspectTol 之内 → 用北美纸张。
//  2. 否则视为 ISO 216（√2）系列，用「长边像素规模」区分 A4 / A3：
//     A4 竖版长边 842pt（300DPI≈3508px）；A3 竖版长边 1191pt（300DPI≈4961px）。
//     取 4200px 为分界，介于两者之间。
//  3. 兜底：默认 A4（用户约定扫描件以 A4 为主）。
//
// 设计取舍：这份工具面向国内政务/企业文档，A4 是绝对主力。
// 因此对「不典型比例」（如 4:3 相机、16:9 截图）一律以 A4 的面积基准换算，
// 而不是硬套一个北美纸张 —— 归一化的目的是让 gs 能算出真实 DPI，
// 只要目标尺寸合理、比例不失真即可。
func scanPaperFor(g pageGeom) paperSpec {
	aspect := g.aspect()
	if aspect <= 0 {
		return scanPaperCandidates[0]
	}

	// 1) 北美系优先判定（仅当比例确实贴近）
	letterDiff := absFloat(aspectLetter - aspect)
	legalDiff := absFloat(aspectLegal - aspect)
	if letterDiff <= naAspectTol && letterDiff <= legalDiff {
		return scanPaperCandidates[2] // Letter
	}
	if legalDiff <= naAspectTol && legalDiff < letterDiff {
		return scanPaperCandidates[3] // Legal
	}

	// 2) ISO 216（√2）系列：用长边规模区分 A4 / A3。
	// A3 竖版长边 1191pt，对应 300DPI 扫描约 4961px；
	// A4 竖版长边 842pt，对应 300DPI 约 3508px。
	// 取 4200px 作为分界（介于两者之间）。
	const a3PixelCut = 4200.0
	if g.maxDim() >= a3PixelCut {
		return scanPaperCandidates[1] // A3
	}
	return scanPaperCandidates[0] // A4
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// classifyPage 对单页页面尺寸做分类
func classifyPage(w, h float64) pageGeom {
	g := pageGeom{w: w, h: h}
	if g.w > g.h {
		g.w, g.h = g.h, g.w // 统一为竖版视角
	}
	if w <= 0 || h <= 0 {
		g.kind = geomUnknown
		return g
	}

	// 1) 先看是否命中标准纸张
	if p, ok := matchStandardPaper(g.w, g.h); ok {
		_ = p
		g.kind = geomStandard
		return g
	}

	// 2) 未命中标准纸张：只有「尺寸明显超过标准纸张上限」才敢判定为像素当 pt。
	//    单纯尺寸略大（如 788×842 的加宽表格）不足以判定，
	//    因为那也可能是合法的自定义页面，强改会破坏版式。
	if g.maxDim() >= oversizeThresholdPt {
		g.kind = geomPixelAsPt
		g.paper = scanPaperFor(g)
		return g
	}

	// 3) 其余一律保守处理：不改动
	g.kind = geomUnknown
	return g
}

// matchStandardPaper 判断页面是否命中某标准纸张（竖版或横版均可，按长边/短边匹配）
func matchStandardPaper(w, h float64) (paperSpec, bool) {
	for _, p := range standardPapers {
		// 竖版匹配
		if relNear(w, p.w) && relNear(h, p.h) {
			return p, true
		}
		// 横版匹配（页面横放，但纸张本身仍是该规格）
		if relNear(w, p.h) && relNear(h, p.w) {
			return p, true
		}
	}
	return paperSpec{}, false
}

// relNear 判断 a 与 b 的相对偏差是否在 paperRelTol 之内
func relNear(a, b float64) bool {
	if b == 0 {
		return false
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d/b <= paperRelTol
}

// hasImageObjects 判断 PDF 是否含图像，采用最宽松的判据（命中任一即算含图）：
//   - XObject 图像：/Subtype/Image 或 /Subtype /Image（允许任意空白/换行）
//   - 内联图像：BI ... ID（PDF 1.0+ 的内联图像语法，按 token 边界识别）
//
// 宁可漏判为「含图」（多跑一次修正）也不要漏判为「无图」
// —— 漏判会让真正需要降采样的扫描件失去修正机会。
func hasImageObjects(data []byte) bool {
	return hasSubtypeImage(data) || hasInlineImage(data)
}

// hasSubtypeImage 查找 /Subtype 后紧跟 /Image（允许任意空白与换行）。
// 兼容 /Subtype/Image 与 /Subtype /Image 两种写法。
func hasSubtypeImage(data []byte) bool {
	key := []byte("/Subtype")
	from := 0
	for {
		i := bytes.Index(data[from:], key)
		if i < 0 {
			return false
		}
		p := skipSpace(data, from+i+len(key))
		from = p
		if p < len(data) && data[p] == '/' {
			p++
		}
		if bytes.HasPrefix(data[p:], []byte("Image")) {
			// 确保是完整的名字 token，避免误配 /ImageMaskFoo 之类
			end := p + len("Image")
			if end >= len(data) || isDelim(data[end]) {
				return true
			}
		}
	}
}

// hasInlineImage 按 token 边界查找内联图像 "BI ... ID"。
// 窗口限制：BI 与 ID 之间是内联图像参数字典，正常远小于 512 字节。
func hasInlineImage(data []byte) bool {
	from := 0
	for {
		i := indexToken(data, "BI", from)
		if i < 0 {
			return false
		}
		from = i + 2
		win := data[from:]
		if len(win) > 512 {
			win = win[:512]
		}
		if indexToken(win, "ID", 0) >= 0 {
			return true
		}
	}
}

// ---------- PDF 解析部分 ----------
//
// 目标：在「不解压整个 PDF、不引入第三方库」的前提下，拿到页面尺寸。
// 做法：
//  1. 优先扫描文件头部的明文 dict（绝大多数 PDF 的页面对象是明文；
//     即便是 xref 流压缩的 PDF，页面对象本身通常也是明文）。
//  2. 若头部未找到，退化为全文扫描（限制读取上限，避免超大文件爆内存）。
//
// 注意：本函数是「尽力而为」——解析失败一律返回 geomUnknown，
// 由调用方走原有逻辑，绝不因为探测失败而影响正常压缩。

// 扫描窗口大小。PDF 的页面对象位置分布有两大类：
//   - 常规「非压缩对象」PDF：页面对象写在文件前部（紧跟在 Catalog/Pages 之后），
//     头部窗口即可覆盖。绝大多数扫描件/办公导出件属于这一类。
//   - 由某些导出器（iText / Skia / 部分扫描仪驱动）产出的「对象流 + 交叉引用流」
//     PDF：页面对象被压缩进 ObjStm，明文 MediaBox 完全不存在于文件里；
//     另有一类把全部页对象堆到文件末尾（实测 test(15) 的 MediaBox 落在
//     21,906,308 / 21,911,041 = 99.98% 处），头部窗口必然漏判。
//
// 因此采用「头 4MiB + 尾 4MiB」双窗口扫描：
//   - 头部覆盖常规 PDF；尾部覆盖「页面对象后置」型 PDF；
//   - 两窗口之间有重叠区去重（见 probePDFGeom），不会重复计页。
//
// 单文件最多读 8MiB，比整文件读入（最大实测 62MB）节省一个数量级的内存与 IO。
const (
	geomScanHead = 4 << 20 // 头部窗口 4 MiB
	geomScanTail = 4 << 20 // 尾部窗口 4 MiB
)

// 窗口读取的重试次数。
//
// 为什么要重试：扫描件常存放在网络盘 / USB / 移动硬盘上，也可能被杀软
// 实时扫描而短暂占用。这类环境下 f.Read 偶发短读或返回瞬时错误，
// 一旦发生在几何探测阶段，就会让该文件静默失去几何修正（见 readDegraded）。
// 重试一次即可消除绝大多数瞬时失败，成本近乎为零。
const readAttempts = 2

// probePDFGeom 探测一个 PDF 文件的页面几何信息。
// 任何错误都不返回 error，而是返回 kind=geomUnknown，保证不影响主流程。
func probePDFGeom(path string) pdfGeom {
	f, err := os.Open(path)
	if err != nil {
		return pdfGeom{kind: geomUnknown}
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return pdfGeom{kind: geomUnknown}
	}
	size := st.Size()
	if size <= 0 {
		return pdfGeom{kind: geomUnknown}
	}

	// ---- 双窗口读取：头部 + 尾部 ----
	// short 文件直接整读；长文件读头尾各 4MiB（中间大段是图像流，无需扫描）。
	// 读取不完整会重试一次；重试后仍不完整则记 degraded，
	// 由调用方提示「该文件的几何判定可能失真」，避免静默降质。
	var head, tail []byte
	var headOff, tailOff int64
	degraded := false
	if size <= int64(geomScanHead+geomScanTail) {
		data, full := readWindow(f, 0, int(size))
		if len(data) == 0 {
			return pdfGeom{kind: geomUnknown}
		}
		head = data
		headOff = 0
		if !full {
			degraded = true
		}
	} else {
		hb, hFull := readWindow(f, 0, geomScanHead)
		if len(hb) == 0 {
			return pdfGeom{kind: geomUnknown}
		}
		head = hb
		headOff = 0
		if !hFull {
			degraded = true
		}

		tailOff = size - int64(geomScanTail)
		tb, tFull := readWindow(f, tailOff, geomScanTail)
		if len(tb) > 0 {
			tail = tb
		} else {
			// 尾部完全读不到：退化为仅头部扫描；偏移不再有意义
			tailOff = 0
		}
		if !tFull {
			degraded = true
		}
	}

	data := head
	if tail != nil {
		// 拼接成单一逻辑缓冲，用 baseOff 标记尾部在逻辑坐标中的起点，
		// 以便「按出现位置去重」时不会把两窗口的同一对象重复计入。
		data = append(append([]byte{}, head...), tail...)
	}

	pages, _ := extractMediaBoxesWithBase(data, len(head), tailOff, headOff)

	// 完全没有明文 MediaBox：可能整份页面对象都在压缩对象流(ObjStm)里。
	// 这类文件无法用明文扫描拿到页面尺寸 → 保守返回 geomUnknown（不改动）。
	if len(pages) == 0 {
		return pdfGeom{kind: geomUnknown, readDegraded: degraded}
	}

	// 统计各尺寸出现情况，取「出现次数最多」的尺寸作为该文件的主尺寸。
	//
	// ⚠ 关键：统计 key 必须对「方向不敏感」——即把 2736×3648 与 3648×2736
	// 视为同一尺寸。原因：混排文档（正文竖版 + 附表横版）极其常见，
	// 若按方向区分，同一规格会被算成两种尺寸，导致：
	//   1. uniform 误判为 false
	//   2. 「主尺寸」被拆票，选出的尺寸不具备代表性
	// 这里统一按「短边×长边」入库，只保留纸张规格本身的信息。
	type key struct{ short, long int }
	counts := map[key]int{}
	for _, s := range pages {
		a, b := s.w, s.h
		if a > b {
			a, b = b, a
		}
		counts[key{int(a + 0.5), int(b + 0.5)}]++
	}

	// 选出主尺寸。
	//
	// ⚠ 必须显式排序后再取，绝不能直接 range map：
	// Go 的 map 遍历顺序是随机的，当多个尺寸的出现次数**并列**时
	// （混排文档极常见，例如仅 2 页却有 2 种尺寸），随机胜出会让
	// 「同一文件、同一输入」在不同运行中被判成不同几何类别 →
	// 分到不同压缩策略分组 → 输出体积忽大忽小、结果不可复现。
	// 实测：test (104).pdf 连探 20 次，16 次判 fix:A4、4 次判 std（两页两种尺寸，并列）。
	//
	// 并列时的确定性规则：先比页数（多者优先），再比长边（大者优先）。
	// 「大者优先」贴合本函数用途 —— 像素当 pt 的扫描页尺寸远大于标准纸张，
	// 并列时优先选中它，才不会漏掉真正需要几何修正的扫描件。
	keys := make([]key, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		if keys[i].long != keys[j].long {
			return keys[i].long > keys[j].long
		}
		return keys[i].short > keys[j].short
	})
	mainW, mainH := float64(keys[0].short), float64(keys[0].long)

	g := classifyPage(mainW, mainH)
	res := pdfGeom{
		kind:         g.kind,
		paper:        g.paper,
		pages:        len(pages),
		uniform:      len(counts) == 1,
		hasImage:     hasImageObjects(data),
		sampleW:      mainW,
		sampleH:      mainH,
		readDegraded: degraded,
	}

	// 统计方向：单独统计「竖版 / 横版」页数，用于
	//  1) 探测方向混排；
	//  2) 决定修正时目标页面的方向（多数方向优先）。
	// 混排时若取错方向，占多数的页面反而会被留白，因此以多数为准。
	var nPortrait, nLandscape int
	for _, s := range pages {
		if s.w <= 0 || s.h <= 0 {
			continue
		}
		if s.h >= s.w {
			nPortrait++
		} else {
			nLandscape++
		}
	}
	res.mixedOrientation = nPortrait > 0 && nLandscape > 0
	// 并列时取竖版（保守默认，与历史行为一致）。
	res.landscape = nLandscape > nPortrait

	// 混合尺寸文件：只有「异常页面占多数」才整体修正；
	// 少数异常页面不动（避免把正常文档搞坏）。
	if res.kind == geomPixelAsPt && !res.uniform {
		abnormal := 0
		for _, s := range pages {
			pg := classifyPage(s.w, s.h)
			if pg.kind == geomPixelAsPt {
				abnormal++
			}
		}
		// 异常页面占比不足一半 → 放弃修正，保守走原逻辑
		if abnormal*2 < len(pages) {
			res.kind = geomUnknown
		}
	}

	return res
}

// readWindow 从文件 off 偏移处读取至多 want 字节。
//
// 与直接 readFull 的区别：读取出错或短读时会重试（最多 readAttempts 次），
// 用于消除网络盘 / 杀软扫描造成的瞬时读取中断。
//
// 返回值：
//
//	data     —— 实际读到的字节（最长 want，可能更短）
//	complete —— 是否一次读满 want 字节。false 表示重试后仍不完整，
//	            调用方应把结果标记为「探测降级」而不是当成完整解析。
func readWindow(f *os.File, off int64, want int) (data []byte, complete bool) {
	if want <= 0 {
		return nil, true
	}
	buf := make([]byte, want)
	best := 0
	for attempt := 0; attempt < readAttempts; attempt++ {
		if _, err := f.Seek(off, 0); err != nil {
			continue
		}
		n, err := readFull(f, buf)
		if n > best {
			best = n
		}
		if err == nil && n == want {
			return buf, true
		}
	}
	return buf[:best], false
}

// readFull 读取至多 len(buf) 字节，返回实际读取数。
// （io.ReadFull 语义，但允许短读而不报错）
func readFull(f *os.File, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			break
		}
	}
	return total, nil
}

// pageSize 页面宽高
type pageSize struct{ w, h float64 }

// extractMediaBoxes 从单一连续字节流中提取所有页面尺寸。
func extractMediaBoxes(data []byte) []pageSize {
	out, _ := extractMediaBoxesWithBase(data, 0, 0, 0)
	return out
}

// extractMediaBoxesWithBase 从「头+尾」拼接缓冲中提取页面尺寸。
//
// 参数：
//
//	data     —— head 与 tail 顺序拼接后的逻辑缓冲
//	headLen  —— head 在 data 中的长度（data[0:headLen] 是头窗口）
//	tailOff  —— tail 在**文件**中的起始偏移（用于换算逻辑坐标）
//	headOff  —— head 在文件中的起始偏移（当前恒为 0，保留以备扩展）
//
// 去重键 = 该 MediaBox 在**文件**中的绝对字节偏移。
// 这样即便同一段文本在头尾窗口各出现一次（长文件的重叠区间），
// 只要绝对偏移不同就会被视为不同页面 —— 与 PDF 里「一页一个对象」的事实一致；
// 而真正被两个窗口重复读到的同一对象，其绝对偏移相同 → 只计一次。
//
// 返回 (页面列表, 未命中数)。未命中数仅用于诊断。
func extractMediaBoxesWithBase(data []byte, headLen int, tailOff, headOff int64) ([]pageSize, int) {
	var out []pageSize
	seenPos := map[int64]bool{}

	// 把「逻辑坐标」换算为「文件绝对偏移」：逻辑坐标 < headLen 的属于头窗口，
	// 否则属于尾窗口（起点为 headLen，对应文件偏移 tailOff）。
	absOf := func(logical int) int64 {
		if logical < headLen {
			return headOff + int64(logical)
		}
		return tailOff + int64(logical-headLen)
	}

	// 1) 内联 MediaBox —— 每个出现位置都算一页
	for _, mb := range findInlineMediaBoxes(data) {
		ap := absOf(mb.pos)
		if seenPos[ap] {
			continue
		}
		seenPos[ap] = true
		out = append(out, mb.size)
	}

	// 2) 间接引用 /MediaBox N 0 R → 解析对象 N 的数组。
	// 先按「引用出现位置」去重（跨窗口防重复），再按对象号去重
	// （同一个尺寸对象被多页共享引用时只计一次）。
	seenRefPos := map[int64]bool{}
	seenRef := map[int]bool{}
	for _, ref := range findMediaBoxRefs(data) {
		ap := absOf(ref.pos)
		if seenRefPos[ap] {
			continue
		}
		seenRefPos[ap] = true
		if seenRef[ref.num] {
			continue
		}
		arr, ok := findObjectArray(data, ref.num)
		if !ok {
			continue
		}
		seenRef[ref.num] = true
		out = append(out, arr)
	}

	return out, 0
}

type mbResult struct {
	doc  string
	size pageSize
	// pos：该 MediaBox 在传入缓冲中的起始字节偏移（用于跨窗口去重）
	pos int
}

// findInlineMediaBoxes 匹配 /MediaBox[ a b c d ]（允许空格/换行分隔）
func findInlineMediaBoxes(data []byte) []mbResult {
	var res []mbResult
	key := []byte("/MediaBox")
	from := 0
	for {
		i := bytes.Index(data[from:], key)
		if i < 0 {
			break
		}
		mbStart := from + i
		p := mbStart + len(key)
		from = p
		// 跳过空白
		for p < len(data) && isSpace(data[p]) {
			p++
		}
		// 必须是 '[' 开头的内联数组
		if p >= len(data) || data[p] != '[' {
			continue
		}
		end := p + 1
		for end < len(data) && data[end] != ']' {
			end++
		}
		if end >= len(data) {
			continue
		}
		body := data[p+1 : end]
		if s, ok := parseFourFloats(body); ok {
			res = append(res, mbResult{doc: string(data[p : end+1]), size: s, pos: mbStart})
		}
	}
	return res
}

// mbRef 一个 /MediaBox N 0 R 引用（含出现位置，用于跨窗口去重）
type mbRef struct {
	num int
	pos int
}

// findMediaBoxRefs 匹配 /MediaBox N 0 R，返回对象号+位置列表
func findMediaBoxRefs(data []byte) []mbRef {
	var refs []mbRef
	key := []byte("/MediaBox")
	from := 0
	for {
		i := bytes.Index(data[from:], key)
		if i < 0 {
			break
		}
		mbStart := from + i
		p := mbStart + len(key)
		from = p
		for p < len(data) && isSpace(data[p]) {
			p++
		}
		if p < len(data) && data[p] == '[' {
			continue // 内联形式，已由上一函数处理
		}
		// 解析 "<num> 0 R"
		num, np, ok := parseUintAt(data, p)
		if !ok {
			continue
		}
		q := np
		q = skipSpace(data, q)
		_, q2, ok := parseUintAt(data, q)
		if !ok {
			continue
		}
		q = skipSpace(data, q2)
		if q < len(data) && data[q] == 'R' {
			refs = append(refs, mbRef{num: int(num), pos: mbStart})
		}
	}
	return refs
}

// findObjectArray 查找 "<objnum> <gen> obj ... [ a b c d ]" 中的数组。
//
// 相比原来的字面量匹配 "<num> 0 obj"，这里按 PDF token 解析：
//   - 允许对象头内任意空白（空格/制表/换行）分隔，如 "7  0  obj"、"7\n0\nobj"；
//   - 允许任意 generation（不限于 0）；
//   - 对象号自带 token 边界，"17 0 obj" 不会被误配到对象 7。
//
// 数组部分取对象体内**第一个能解析成四个数**的 '[' 数组，
// 避免命中对象头里可能出现的其它非数值数组。
func findObjectArray(data []byte, objnum int) (pageSize, bool) {
	numTok := itoa(objnum)
	from := 0
	for {
		i := indexToken(data, numTok, from)
		if i < 0 {
			return pageSize{}, false
		}
		from = i + len(numTok)

		// "num gen obj"
		q := skipSpace(data, from)
		_, np, ok := parseUintAt(data, q)
		if !ok {
			continue
		}
		q = skipSpace(data, np)
		if !bytes.HasPrefix(data[q:], []byte("obj")) {
			continue
		}
		endObj := q + len("obj")
		if endObj < len(data) && !isDelim(data[endObj]) {
			continue
		}
		q = endObj

		// 对象体内第一个可解析为四数的数组
		win := data[q:]
		if len(win) > 4096 {
			win = win[:4096]
		}
		for s := bytes.IndexByte(win, '['); s >= 0; {
			e := s + 1
			for e < len(win) && win[e] != ']' {
				e++
			}
			if e >= len(win) {
				return pageSize{}, false
			}
			if sz, ok := parseFourFloats(win[s+1 : e]); ok {
				return sz, true
			}
			off := bytes.IndexByte(win[e+1:], '[')
			if off < 0 {
				return pageSize{}, false
			}
			s = e + 1 + off
		}
		return pageSize{}, false
	}
}

// parseFourFloats 解析 "a b c d" 四个数，返回宽高（x1-x0, y1-y0）
func parseFourFloats(b []byte) (pageSize, bool) {
	vals := make([]float64, 0, 4)
	i := 0
	for i < len(b) && len(vals) < 4 {
		for i < len(b) && isSpace(b[i]) {
			i++
		}
		start := i
		for i < len(b) && !isSpace(b[i]) && b[i] != ']' {
			i++
		}
		if i == start {
			break
		}
		v, err := parseFloatBytes(b[start:i])
		if err != nil {
			return pageSize{}, false
		}
		vals = append(vals, v)
	}
	if len(vals) != 4 {
		return pageSize{}, false
	}
	return pageSize{w: vals[2] - vals[0], h: vals[3] - vals[1]}, true
}

// parseUintAt 从位置 p 解析一个非负整数
func parseUintAt(data []byte, p int) (uint64, int, bool) {
	start := p
	for p < len(data) && data[p] >= '0' && data[p] <= '9' {
		p++
	}
	if p == start {
		return 0, start, false
	}
	v, err := strconvParseUint(data[start:p])
	if err != nil {
		return 0, start, false
	}
	return v, p, true
}

// ---- 极简的数值解析（避免引入 strconv 之外的开销与依赖）----
// 说明：这里直接手写是为了 1) 不走 locale 2) 允许「1.」这类不完整写法容错。

func strconvParseUint(b []byte) (uint64, error) {
	return parseUintManual(b)
}

func parseUintManual(b []byte) (uint64, error) {
	var v uint64
	if len(b) == 0 {
		return 0, errNum{}
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, errNum{}
		}
		v = v*10 + uint64(c-'0')
		if v > 1<<40 {
			return 0, errNum{}
		}
	}
	return v, nil
}

type errNum struct{}

func (errNum) Error() string { return "invalid number" }

// parseFloatBytes 解析浮点数（含负号与小数点）
func parseFloatBytes(b []byte) (float64, error) {
	if len(b) == 0 {
		return 0, errNum{}
	}
	neg := false
	i := 0
	if b[0] == '-' || b[0] == '+' {
		neg = b[0] == '-'
		i++
	}
	var intPart, frac uint64
	var fracDiv float64 = 1
	seenDigit := false
	for ; i < len(b) && b[i] >= '0' && b[i] <= '9'; i++ {
		intPart = intPart*10 + uint64(b[i]-'0')
		seenDigit = true
	}
	if i < len(b) && b[i] == '.' {
		i++
		for ; i < len(b) && b[i] >= '0' && b[i] <= '9'; i++ {
			frac = frac*10 + uint64(b[i]-'0')
			fracDiv *= 10
			seenDigit = true
		}
	}
	if !seenDigit || i != len(b) {
		return 0, errNum{}
	}
	v := float64(intPart) + float64(frac)/fracDiv
	if neg {
		v = -v
	}
	return v, nil
}

// itoa 无依赖整数转字符串（仅用于构造对象号匹配串）
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// isSpace PDF 中用于分隔 token 的空白字符
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == 0
}

// isDelim 判断字节是否为 PDF token 分隔符（空白或 PDF 定界符）。
func isDelim(c byte) bool {
	if isSpace(c) {
		return true
	}
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

// skipSpace 返回 data 中从 p 起跳过空白后的位置。
func skipSpace(data []byte, p int) int {
	for p < len(data) && isSpace(data[p]) {
		p++
	}
	return p
}

// indexToken 在 data[from:] 中查找「独立 token」tok（前后均为空白或 PDF 定界符）。
// 用于避免把 "7" 误配到 "17"、把 "obj" 误配到 "objective" 之类。
// 找不到返回 -1。
func indexToken(data []byte, tok string, from int) int {
	needle := []byte(tok)
	if len(needle) == 0 || from < 0 {
		return -1
	}
	for from <= len(data)-len(needle) {
		i := bytes.Index(data[from:], needle)
		if i < 0 {
			return -1
		}
		p := from + i
		end := p + len(needle)
		left := p == 0 || isDelim(data[p-1])
		right := end == len(data) || isDelim(data[end])
		if left && right {
			return p
		}
		from = p + 1
	}
	return -1
}
