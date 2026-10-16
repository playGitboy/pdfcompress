package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 空文件必须在调用 Ghostscript 之前被判失败：
// gs 对 0 字节输入会写出一个空白 PDF 并以退出码 0 结束，若走体积安全网
// 会被误报为「KEEP 成功」。守卫生效时不应执行 gs，因此传入不存在的路径即可。
func TestCompressFileRejectsEmptyInput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "empty.pdf")
	if err := os.WriteFile(in, nil, 0644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "o.pdf")
	err := compressFile("/nonexistent/gs-binary", in, out, LevelEBook, 1)
	if err == nil || !strings.Contains(err.Error(), "为空") {
		t.Fatalf("空文件应被判失败，实得 err=%v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("空文件失败时不应产生输出文件")
	}
}

// 目录作为输入同样要给出明确错误，而不是让 gs 抛 /ioerror。
func TestCompressFileRejectsDirInput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "adir")
	if err := os.Mkdir(in, 0755); err != nil {
		t.Fatal(err)
	}
	err := compressFile("/nonexistent/gs-binary", in, filepath.Join(dir, "o.pdf"), LevelEBook, 1)
	if err == nil || !strings.Contains(err.Error(), "目录") {
		t.Fatalf("目录输入应被判失败，实得 err=%v", err)
	}
}

// 多文件拖放：若某个文件的输出名正好等于本次另一个源文件名，
// 并发/分块写入会覆盖尚未处理的源文件，必须直接拒绝（返回 -1）。
// 这道拦截发生在调用 gs 之前，因此传入不存在的 gs 路径即可测试。
func TestRunFilesRejectsOutputInputCollision(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.pdf")
	b := filepath.Join(dir, "a_ebook.pdf")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("%PDF-1.4\n1 0 obj <</Type/Page/MediaBox[0 0 595 842]>> endobj\n%%EOF\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	rc := runFiles("/nonexistent/gs-binary", []string{a, b}, LevelEBook, 1, true, 1, true, false, 0, false)
	if rc != -1 {
		t.Fatalf("输出与输入同名时应返回 -1，实得 %d", rc)
	}
}

// cleanStaleOutputs 只清理「不存在或 0 字节」的无效产物，有效产物必须保留。
// 对应现象：多文件单进程批量失败/被终止后，gs 已创建但未写入的输出文件
// 会残留为 0 字节，必须清掉，否则输出目录里会出现「空 PDF」。
func TestCleanStaleOutputs(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.pdf")
	empty := filepath.Join(dir, "empty.pdf")
	missing := filepath.Join(dir, "missing.pdf")
	if err := os.WriteFile(valid, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, nil, 0644); err != nil {
		t.Fatal(err)
	}

	cleanStaleOutputs([]string{valid, empty, missing})

	if _, err := os.Stat(valid); err != nil {
		t.Errorf("有效产物不应被删除: %v", err)
	}
	for _, p := range []string{empty, missing} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s 应已被清理", filepath.Base(p))
		}
	}
}

// removeQuiet 对不存在的路径必须立即返回（不重试、不报错、不 panic）。
func TestRemoveQuietMissingPath(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	removeQuiet(filepath.Join(dir, "not-there.tmp"))
	if d := time.Since(start); d > time.Second {
		t.Errorf("不存在的路径应快速返回，实际耗时 %s", d)
	}
}

// removeQuiet 应真正删除存在的文件。
func TestRemoveQuietDeletesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".pdfc1_0.tmp")
	if err := os.WriteFile(p, nil, 0644); err != nil {
		t.Fatal(err)
	}
	removeQuiet(p)
	if _, err := os.Stat(p); err == nil {
		t.Error("文件应已被删除")
	}
}

// 拖入文件夹的扫描必须递归，并跳过历史遗留的 compressed/ 输出目录——
// 否则上一轮集中输出的结果会被当成源文件再压一遍。
func TestScanFolderPDFsRecursiveAndSkipsCompressed(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	legacy := filepath.Join(dir, "compressed")
	for _, d := range []string{sub, legacy} {
		if err := os.Mkdir(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{
		filepath.Join(dir, "a.pdf"),
		filepath.Join(sub, "b.pdf"),
		filepath.Join(legacy, "old.pdf"), // 旧版集中输出，必须跳过
		filepath.Join(dir, "note.txt"),   // 非 PDF，必须跳过
	} {
		if err := os.WriteFile(p, []byte("%PDF-1.4\n%%EOF\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	pdfs, unreadable, err := scanFolderPDFs(dir)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(unreadable) != 0 {
		t.Errorf("不应有不可读项，实得 %v", unreadable)
	}

	abs, _ := filepath.Abs(dir)
	want := []string{filepath.Join(abs, "a.pdf"), filepath.Join(abs, "sub", "b.pdf")}
	if len(pdfs) != len(want) {
		t.Fatalf("应找到 %d 个PDF，实得 %d: %v", len(want), len(pdfs), pdfs)
	}
	for i := range want {
		if !samePath(pdfs[i], want[i]) {
			t.Errorf("第 %d 个应为 %s，实得 %s", i, want[i], pdfs[i])
		}
	}
	for _, p := range pdfs {
		if !filepath.IsAbs(p) {
			t.Errorf("应返回绝对路径（便于去重），实得 %s", p)
		}
	}
}

// 文件夹扫描要剔除本级别上一次的输出，否则重复拖放同一文件夹时，
// 「原名.pdf」的输出名会与本级别旧输出同名，触发冲突保护而使整批中止。
func TestFilterSelfOutputs(t *testing.T) {
	in := []string{
		"报告.pdf",           // 源文件，保留
		"报告_ebook.pdf",     // 本级别旧输出，剔除
		"报告_printer.pdf",   // 别的级别，保留（本次是 ebook）
		"报表_副本.pdf",        // 源文件，保留
		"_ebook.pdf",       // 主体不长于后缀，保留（避免误判）
		"REPORT_EBOOK.PDF", // 大小写不敏感，剔除
	}
	kept, skipped := filterSelfOutputs(in, LevelEBook)
	if skipped != 2 {
		t.Errorf("应剔除 2 个，实得 %d", skipped)
	}
	want := []string{"报告.pdf", "报告_printer.pdf", "报表_副本.pdf", "_ebook.pdf"}
	if len(kept) != len(want) {
		t.Fatalf("应保留 %d 个，实得 %d: %v", len(want), len(kept), kept)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Errorf("第 %d 个应为 %s，实得 %s", i, want[i], kept[i])
		}
	}
}
