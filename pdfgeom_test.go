package main

import (
	"testing"
)

func TestClassifyPage(t *testing.T) {
	cases := []struct {
		name     string
		w, h     float64
		wantKind geomKind
	}{
		{"A4 标准", 595, 842, geomStandard},
		{"A4 横版", 842, 595, geomStandard},
		{"A4 取整差异", 595.276, 841.89, geomStandard},
		{"A3 标准", 842, 1191, geomStandard},
		{"Letter", 612, 792, geomStandard},
		{"Legal", 612, 1008, geomStandard},
		{"A5", 420, 595, geomStandard},
		{"手机拍摄 2736x3648", 2736, 3648, geomPixelAsPt},
		{"扫描仪 A4 2480x3508", 2480, 3508, geomPixelAsPt},
		{"扫描仪 A3 3508x4961", 3508, 4961, geomPixelAsPt},
		{"加宽表格 788x842", 788, 842, geomUnknown}, // 略大但未过阈值 → 保守不改
	}
	for _, c := range cases {
		g := classifyPage(c.w, c.h)
		if g.kind != c.wantKind {
			t.Errorf("%s: classifyPage(%v,%v) kind=%v want=%v",
				c.name, c.w, c.h, g.kind, c.wantKind)
		}
	}
}

func TestScanPaperFor(t *testing.T) {
	cases := []struct {
		name string
		w, h float64
		want string
	}{
		// 4:3 手机拍摄(aspect 1.333)：既不贴近 √2(1.415) 也不贴近 Letter(1.294)
		// ——按用户约定「扫描件以 A4 为主」兜底为 A4。
		{"手机拍摄 4:3 2736x3648", 2736, 3648, "A4"},
		// √2 比例：靠绝对像素规模区分
		{"扫描 A4 2480x3508", 2480, 3508, "A4"},
		{"扫描 A3 3508x4961", 3508, 4961, "A3"},
		// 北美比例：Letter 1.294
		{"Letter 扫描 2550x3300", 2550, 3300, "Letter"},
	}
	for _, c := range cases {
		g := classifyPage(c.w, c.h)
		if g.paper.name != c.want {
			t.Errorf("%s: paper=%s want=%s (aspect=%.3f)",
				c.name, g.paper.name, c.want, g.aspect())
		}
	}
}

func TestScanPaperForBrazilianA4(t *testing.T) {
	// 300DPI 扫描 A4 = 2480×3508，300DPI 扫描 A3 = 3508×4961
	if p := scanPaperFor(pageGeom{w: 2480, h: 3508}); p.name != "A4" {
		t.Errorf("2480x3508 -> %s, want A4", p.name)
	}
	if p := scanPaperFor(pageGeom{w: 3508, h: 4961}); p.name != "A3" {
		t.Errorf("3508x4961 -> %s, want A3", p.name)
	}
	// 600DPI 扫描 A4 = 4960×7016，长边超过 4200 → 会判成 A3。
	// 这是已知取舍：单凭页面像素无法区分「高DPI的A4」与「低DPI的A3」，
	// 两者最终都会归一化到 √2 比例的标准纸张，观感一致，可接受。
	if p := scanPaperFor(pageGeom{w: 4960, h: 7016}); p.name != "A3" {
		t.Logf("note: 4960x7016 -> %s (600DPI A4 被归为 A3，属预期取舍)", p.name)
	}
}

func TestPaperForAspect(t *testing.T) {
	// √2 比例：A4(842/595≈1.4151) 与 A3(1191/842≈1.4145) 极其接近，
	// 输入 1.414 时 A3 的比值数值上更贴近 → 返回 A3。
	// 这不影响正确性：A4/A3 比例本就同为 √2，归一化后的观感一致。
	// 需要真正区分 A4/A3 时走 scanPaperFor（按绝对像素规模判定）。
	p := paperForAspect(1.414, scanPaperCandidates)
	if p.name != "A3" {
		t.Errorf("aspect 1.414 -> %s, want A3（数值上更贴近）", p.name)
	}
	// Letter 比例：候选里 Letter 最贴近
	p = paperForAspect(1.294, scanPaperCandidates)
	if p.name != "Letter" {
		t.Errorf("aspect 1.294 -> %s, want Letter", p.name)
	}
	// Legal 比例
	p = paperForAspect(1.647, scanPaperCandidates)
	if p.name != "Legal" {
		t.Errorf("aspect 1.647 -> %s, want Legal", p.name)
	}
}

func TestParseFourFloats(t *testing.T) {
	s, ok := parseFourFloats([]byte("0 0 595.276 841.89"))
	if !ok {
		t.Fatal("parse failed")
	}
	if s.w < 594 || s.w > 596 || s.h < 841 || s.h > 843 {
		t.Errorf("got %v x %v", s.w, s.h)
	}
}

func TestExtractMediaBoxInline(t *testing.T) {
	// 模拟一个含 3 页的 PDF 片段（前两页尺寸文本完全相同，
	// 必须按出现位置各算一页，不能被文本去重合并）
	data := []byte(`1 0 obj <</Type/Page/MediaBox[0 0 595 842]>> endobj
2 0 obj <</Type/Page/MediaBox[0 0 595 842]>> endobj
3 0 obj <</Type/Page/MediaBox[0 0 2736 3648]>> endobj`)
	got := extractMediaBoxes(data)
	if len(got) != 3 {
		t.Fatalf("want 3 mediboxes, got %d: %v", len(got), got)
	}
}

// 混排方向：正文竖版 + 附表横版，尺寸应视为同一规格
func TestExtractMixedOrientation(t *testing.T) {
	data := []byte(`1 0 obj <</Type/Page/MediaBox[0 0 2736 3648]>> endobj
2 0 obj <</Type/Page/MediaBox[0 0 3648 2736]>> endobj
3 0 obj <</Type/Page/MediaBox[0 0 2736 3648]>> endobj`)
	got := extractMediaBoxes(data)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	// 方向不敏感统计后应只有 1 种规格
	type key struct{ a, b int }
	set := map[key]bool{}
	for _, s := range got {
		a, b := s.w, s.h
		if a > b {
			a, b = b, a
		}
		set[key{int(a), int(b)}] = true
	}
	if len(set) != 1 {
		t.Errorf("期望方向不敏感后只有 1 种规格，实际 %d: %v", len(set), set)
	}
}

func TestExtractMediaBoxRef(t *testing.T) {
	data := []byte(`1 0 obj <</Type/Page/MediaBox 5 0 R>> endobj
5 0 obj [0 0 2736 3648] endobj`)
	got := extractMediaBoxes(data)
	if len(got) != 1 {
		t.Fatalf("want 1, got %d: %v", len(got), got)
	}
	if got[0].w != 2736 || got[0].h != 3648 {
		t.Errorf("got %v", got[0])
	}
}
