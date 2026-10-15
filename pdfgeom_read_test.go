package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestReadWindow 覆盖几何探测的窗口读取：
// 完整读取、偏移读取、请求超出文件长度（短读）、越界读取四种情形。
// 其中「短读必须返回 complete=false」是硬要求 —— 它正是探测降级提示的触发条件。
func TestReadWindow(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "d.bin")
	src := make([]byte, 1000)
	for i := range src {
		src[i] = byte(i % 251)
	}
	if err := os.WriteFile(p, src, 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// 1) 从头部读满
	got, ok := readWindow(f, 0, 600)
	if !ok || len(got) != 600 {
		t.Fatalf("完整读取: ok=%v len=%d，期望 true/600", ok, len(got))
	}
	if !bytes.Equal(got, src[:600]) {
		t.Error("头部读到的数据不一致")
	}

	// 2) 从中间偏移读满
	got, ok = readWindow(f, 400, 600)
	if !ok || len(got) != 600 {
		t.Fatalf("偏移读取: ok=%v len=%d，期望 true/600", ok, len(got))
	}
	if !bytes.Equal(got, src[400:1000]) {
		t.Error("偏移读到的数据不一致")
	}

	// 3) 请求超过文件长度 → 短读：必须 complete=false，且返回已读到的部分
	got, ok = readWindow(f, 0, 2000)
	if ok {
		t.Error("请求超过文件长度时应返回 complete=false")
	}
	if len(got) != 1000 {
		t.Errorf("短读应返回已读到的 1000 字节，实得 %d", len(got))
	}

	// 4) 越界 seek：读不到数据，但不应 panic，complete=false
	got, ok = readWindow(f, 5000, 100)
	if ok || len(got) != 0 {
		t.Errorf("越界读取: ok=%v len=%d，期望 false/0", ok, len(got))
	}
}

// TestProbePDFGeomNotDegraded 确认正常本地文件不会被误标为「探测降级」
// （否则会产生虚假提示，干扰用户）。
func TestProbePDFGeomNotDegraded(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.pdf")
	// 一个含 MediaBox 的极简 PDF 骨架（不是合法 PDF，但对几何探测足够）
	content := "%PDF-1.4\n1 0 obj <</Type/Page/MediaBox[0 0 595 842]>> endobj\n%%EOF\n"
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	g := probePDFGeom(p)
	if g.readDegraded {
		t.Error("正常本地文件不应被标记为读取不完整")
	}
	if g.pages != 1 {
		t.Errorf("应探测到 1 个 MediaBox，实得 %d", g.pages)
	}
}

// TestProbePDFGeomDeterministic 回归测试：页面尺寸**并列**时必须给出稳定结果。
//
// 背景（2026-10-15 实测复现的真实缺陷）：
// 主尺寸原来是直接 range map 取「出现次数最多」者。Go 的 map 遍历顺序随机，
// 当多种尺寸页数并列时（例如仅 2 页却有 2 种尺寸），随机胜出 →
// 同一文件在不同运行中被判成不同几何类别 → 分到不同压缩分组 →
// 输出体积忽大忽小。实测 test (104).pdf 连探 20 次，16 次 fix:A4、4 次 std。
//
// 本测试用「2 页 2 种尺寸」的合成样本连探多次，要求结果完全一致。
func TestProbePDFGeomDeterministic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tie.pdf")
	// 2 页、2 种尺寸：一次并列。若选主尺寸仍依赖 map 顺序，多次探测必然漂移。
	content := "%PDF-1.4\n" +
		"1 0 obj <</Type/Page/MediaBox[0 0 595 842]>> endobj\n" +
		"2 0 obj <</Type/Page/MediaBox[0 0 2736 3648]>> endobj\n" +
		"%%EOF\n"
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	first := probePDFGeom(p)
	firstKey := geomGroupKey(first)
	for i := 0; i < 50; i++ {
		g := probePDFGeom(p)
		if k := geomGroupKey(g); k != firstKey {
			t.Fatalf("第 %d 次探测结果漂移: %q != %q（主尺寸选择不确定）", i+1, k, firstKey)
		}
		if g.sampleW != first.sampleW || g.sampleH != first.sampleH || g.kind != first.kind {
			t.Fatalf("第 %d 次探测细节漂移: %.0fx%.0f kind=%v（首测 %.0fx%.0f kind=%v）",
				i+1, g.sampleW, g.sampleH, g.kind, first.sampleW, first.sampleH, first.kind)
		}
	}

	// 并列时按「长边更大者优先」：应选中 2736x3648（像素当 pt 的扫描页）
	if first.sampleW != 2736 || first.sampleH != 3648 {
		t.Errorf("并列时应选长边更大的尺寸 2736x3648，实得 %.0fx%.0f", first.sampleW, first.sampleH)
	}
}
