package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 几何基准修正必须保持原始页面方向：
// 竖版文件用 (短边, 长边)，横版文件对调，否则横版内容会被压进竖版页面、
// 上下各留一大片白边（实测横版内容只剩约 70% 线性尺寸）。
func TestGeometryArgsKeepsOrientation(t *testing.T) {
	base := pdfGeom{
		kind:     geomPixelAsPt,
		hasImage: true,
		paper:    paperSpec{"A4", 595, 842},
		sampleW:  2480,
		sampleH:  3508,
	}
	portrait := base
	land := base
	land.landscape = true

	pArgs := strings.Join(geometryArgs(portrait, LevelEBook), " ")
	lArgs := strings.Join(geometryArgs(land, LevelEBook), " ")

	if !strings.Contains(pArgs, "-dDEVICEWIDTHPOINTS=595.26") ||
		!strings.Contains(pArgs, "-dDEVICEHEIGHTPOINTS=842.00") {
		t.Errorf("竖版设备尺寸错误: %s", pArgs)
	}
	if !strings.Contains(lArgs, "-dDEVICEWIDTHPOINTS=842.00") ||
		!strings.Contains(lArgs, "-dDEVICEHEIGHTPOINTS=595.26") {
		t.Errorf("横版设备尺寸错误: %s", lArgs)
	}

	// 分组键必须区分方向：同一 gs 进程的设备宽高是进程级的，
	// 竖版与横版混进同一 chunk 必然有一方被套错方向。
	if geomGroupKey(portrait) == geomGroupKey(land) {
		t.Errorf("竖版/横版的分组键相同: %s", geomGroupKey(portrait))
	}
}

// 对象头解析必须容忍任意空白与任意 generation，且不能把对象 7
// 误配到对象 17 上。
func TestFindObjectArrayRobust(t *testing.T) {
	cases := []struct {
		doc   string
		wantW float64
		wantH float64
	}{
		{"7 0 obj\n[0 0 100 200]\nendobj", 100, 200},
		{"7  0  obj  <</X 1>> [0 0 101 201] endobj", 101, 201},
		{"7\n0\nobj\n<<>>\n[0 0 102 202]\nendobj", 102, 202},
		{"7 3 obj [0 0 103 203] endobj", 103, 203},                    // 非 0 generation
		{"7 0 obj <</A [true null]>> [0 0 300 400] endobj", 300, 400}, // 跳过非数值数组
	}
	for i, c := range cases {
		sz, ok := findObjectArray([]byte(c.doc), 7)
		if !ok {
			t.Fatalf("case %d 未解析到数组: %q", i, c.doc)
		}
		if sz.w != c.wantW || sz.h != c.wantH {
			t.Errorf("case %d got %vx%v want %vx%v", i, sz.w, sz.h, c.wantW, c.wantH)
		}
	}

	if _, ok := findObjectArray([]byte("17 0 obj [0 0 999 999] endobj"), 7); ok {
		t.Error("对象 7 被误配到对象 17")
	}
}

// 含图判定：/Subtype 与 /Image 之间的任意空白都要命中；内联图像按 token 识别；
// 不含图像的文本/字体对象不得误判。
func TestHasImageObjectsRobust(t *testing.T) {
	yes := [][]byte{
		[]byte("<</Type/XObject/Subtype/Image/Width 10>>"),
		[]byte("<</Subtype /Image /Width 10>>"),
		[]byte("<</Subtype\n/Image>>"),
		[]byte("q 100 0 0 100 0 0 cm BI /W 4 /H 4 /BPC 8 /CS /G ID \x00\x01 EI Q"),
	}
	for i, b := range yes {
		if !hasImageObjects(b) {
			t.Errorf("case %d 应判为含图: %q", i, b)
		}
	}
	no := [][]byte{
		[]byte("%PDF-1.4 1 0 obj <</Type/Page/MediaBox[0 0 595 842]>> endobj"),
		[]byte("<</Subtype/Type1/BaseFont/Arial>>"),
	}
	for i, b := range no {
		if hasImageObjects(b) {
			t.Errorf("case %d 不应判为含图: %q", i, b)
		}
	}
}

// 方向混排时按「多数方向」定目标页面，避免占多数的页面反而被留白。
func TestProbeMixedOrientationMajority(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.pdf")
	content := "%PDF-1.4\n" +
		"1 0 obj <</Type/Page/MediaBox[0 0 3508 2480]>> endobj\n" +
		"2 0 obj <</Type/Page/MediaBox[0 0 3508 2480]>> endobj\n" +
		"3 0 obj <</Type/Page/MediaBox[0 0 2480 3508]>> endobj\n" +
		"%%EOF\n"
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	g := probePDFGeom(p)
	if !g.mixedOrientation {
		t.Error("应判为方向混排")
	}
	if !g.landscape {
		t.Error("横版占多数，应 landscape=true")
	}
}

// 横版像素当 pt 文件：优先验证 shouldFixGeometry 与分组键方向一致。
func TestLandscapePixelAsPt(t *testing.T) {
	g := classifyPage(3508, 2480)
	if g.kind != geomPixelAsPt {
		t.Fatalf("3508x2480 应为 geomPixelAsPt，实得 %v", g.kind)
	}
}

// 输出路径与输入路径相同的判定：必须能识别同一路径的不同写法与符号链接，
// 以便在任何模式下拦下「写回源文件」的数据丢失路径。
func TestSamePath(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.pdf")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if !samePath(f, f) {
		t.Error("同一路径应判为相同")
	}
	if !samePath(filepath.Join(dir, "..", filepath.Base(dir), "a.pdf"), f) {
		t.Error("含 .. 的相对路径应归一化后判为相同")
	}
	if samePath(f, filepath.Join(dir, "b.pdf")) {
		t.Error("不同文件不应判为相同")
	}
	link := filepath.Join(dir, "link.pdf")
	if err := os.Symlink(f, link); err == nil {
		if !samePath(link, f) {
			t.Error("符号链接应判为同一文件")
		}
	}
}
