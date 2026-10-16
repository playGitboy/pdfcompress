package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 含书签/命名目标的文件必须被识别（防 pdfmark 跨文档泄漏导致同进程串联卡死）。
func TestPdfHasDestMarkersDetected(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"书签", "1 0 obj << /Type /Catalog /Outlines 2 0 R >> endobj", true},
		{"命名目标", "3 0 obj << /Dests 4 0 R >> endobj", true},
		{"跳转动作", "5 0 obj << /S /GoTo /D [6 0 R /XYZ 0 842 null] >> endobj", true},
		{"纯扫描件（无任何特征）", "%PDF-1.4 1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "t.pdf")
			if err := os.WriteFile(p, []byte(c.body), 0644); err != nil {
				t.Fatal(err)
			}
			if got := pdfHasDestMarkers(p); got != c.want {
				t.Errorf("pdfHasDestMarkers = %v, 期望 %v", got, c.want)
			}
		})
	}
}

// 特征串恰好横跨 1MiB 窗口边界时也必须命中（靠窗口间 16 字节重叠）。
func TestPdfHasDestMarkersSpanWindowBoundary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "span.pdf")
	// 构造：填充使 "/Out" 落在第 1 窗口末尾、"lines" 落在第 2 窗口开头
	front := strings.Repeat("\x00", 1<<20-4) // 第 1 窗口最后 4 字节留给 "/Out"
	body := front + "/Outlines 2 0 R"
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if !pdfHasDestMarkers(p) {
		t.Error("跨窗口边界的 /Outlines 特征应被识别")
	}
}

// 文件打不开时按「有特征」处理（宁可独占进程，也不冒卡死风险）。
func TestPdfHasDestMarkersUnreadable(t *testing.T) {
	if !pdfHasDestMarkers(filepath.Join(t.TempDir(), "not-exist.pdf")) {
		t.Error("无法打开的文件应按有特征处理")
	}
}

// scanDestMarkers 返回结果必须与输入等长且逐文件对应。
func TestScanDestMarkers(t *testing.T) {
	dir := t.TempDir()
	clean := filepath.Join(dir, "clean.pdf")
	marked := filepath.Join(dir, "marked.pdf")
	if err := os.WriteFile(clean, []byte("%PDF-1.4 plain"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marked, []byte("<< /Outlines 2 0 R >>"), 0644); err != nil {
		t.Fatal(err)
	}
	got := scanDestMarkers([]string{clean, marked}, 8)
	if len(got) != 2 {
		t.Fatalf("结果长度 = %d, 期望 2", len(got))
	}
	if got[0] || !got[1] {
		t.Errorf("scanDestMarkers = %v, 期望 [false true]", got)
	}
}
