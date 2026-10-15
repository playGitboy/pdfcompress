package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
