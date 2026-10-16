package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// captureStdout 临时替换 os.Stdout 收集输出，结束后恢复并返回收集内容。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()
	w.Close()
	buf := make([]byte, 64<<10)
	n, _ := r.Read(buf)
	return string(buf[:n])
}

// 静默超过阈值后必须出现「仍在处理」心跳；stop 后行被清除且无残留输出。
func TestStartHeartbeatPrintsAfterSilence(t *testing.T) {
	oldIsatty, oldAfter, oldInterval := isattyStdout, hbAfter, hbInterval
	isattyStdout = func() bool { return true }
	hbAfter = 200 * time.Millisecond
	hbInterval = 50 * time.Millisecond
	defer func() { isattyStdout, hbAfter, hbInterval = oldIsatty, oldAfter, oldInterval }()

	var out string
	done := make(chan struct{})
	go func() {
		out = captureStdout(t, func() {
			stop := startHeartbeat("大文件.pdf", false)
			time.Sleep(700 * time.Millisecond) // 覆盖至少两次心跳刷新
			stop()
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("心跳未按时结束")
	}

	if !strings.Contains(out, "仍在处理") || !strings.Contains(out, "大文件.pdf") {
		t.Errorf("输出中缺少心跳提示, 实得: %q", out)
	}
	// 心跳含 \r 原位刷新与清除用的空格
	if !strings.Contains(out, "\r") {
		t.Error("心跳应使用 \\r 原位刷新")
	}
}

// quiet 模式下心跳必须完全静默（-q 是脚本调用契约，不能有额外输出）。
func TestStartHeartbeatQuiet(t *testing.T) {
	oldIsatty, oldAfter := isattyStdout, hbAfter
	isattyStdout = func() bool { return true }
	hbAfter = 100 * time.Millisecond
	defer func() { isattyStdout, hbAfter = oldIsatty, oldAfter }()

	var out string
	done := make(chan struct{})
	go func() {
		out = captureStdout(t, func() {
			stop := startHeartbeat("x.pdf", true)
			time.Sleep(500 * time.Millisecond)
			stop()
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("心跳未按时结束")
	}
	if out != "" {
		t.Errorf("quiet 模式不应有任何心跳输出, 实得: %q", out)
	}
}

// 非交互式控制台（重定向）下心跳必须完全静默，避免 \r 污染日志。
func TestStartHeartbeatRedirected(t *testing.T) {
	oldIsatty, oldAfter := isattyStdout, hbAfter
	isattyStdout = func() bool { return false }
	hbAfter = 100 * time.Millisecond
	defer func() { isattyStdout, hbAfter = oldIsatty, oldAfter }()

	var out string
	done := make(chan struct{})
	go func() {
		out = captureStdout(t, func() {
			stop := startHeartbeat("x.pdf", false)
			time.Sleep(500 * time.Millisecond)
			stop()
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("心跳未按时结束")
	}
	if out != "" {
		t.Errorf("重定向时不应有任何心跳输出, 实得: %q", out)
	}
}

// 心跳提示必须限制在一行内。Win7 控制台默认 80 列，提示一旦超宽就会折行，
// 紧接着的清除空格同样超宽再折行一次 —— 表现为每刷新一次多出一行空白。
func TestHeartbeatMsgFitsConsoleWidth(t *testing.T) {
	oldWidth := consoleWidth
	consoleWidth = func() int { return 80 }
	defer func() { consoleWidth = oldWidth }()

	limit := hbLineWidth()
	if limit != 79 {
		t.Fatalf("80 列控制台应留 1 列余量，可用 79 列，实得 %d", limit)
	}
	name := "2023年中华人民共和国海关进出口税则（第一批修订版）最终稿.pdf"
	tail := "，已 " + (2*time.Minute + 30*time.Second).String()
	msg := buildHeartbeatMsg(name, 40, tail, limit)
	if got := displayWidth(msg); got > limit {
		t.Fatalf("提示宽度 %d 超过可用宽度 %d: %q", got, limit, msg)
	}
	if !strings.Contains(msg, "2m30s") {
		t.Errorf("缩短文件名后仍应保留耗时信息, 实得: %q", msg)
	}
	if !strings.HasPrefix(msg, "仍在处理: ") {
		t.Errorf("提示前缀缺失: %q", msg)
	}
}

// 控制台宽度不明确（非 Windows / 获取失败）时按 Win7 默认 80 列兜底。
func TestHeartbeatLineWidthFallback(t *testing.T) {
	oldWidth := consoleWidth
	consoleWidth = func() int { return 0 }
	defer func() { consoleWidth = oldWidth }()
	if got := hbLineWidth(); got != 79 {
		t.Fatalf("宽度未知时应按 80 列兜底（可用 79 列），实得 %d", got)
	}
}

// 收窄窗口下，输出的每一帧（\r 分隔）都不得超过可用列数，
// 且清除用的空格数正好等于该帧宽度：既不留残影，也不因超宽折行。
func TestHeartbeatFramesFitConsoleWidth(t *testing.T) {
	oldIsatty, oldAfter, oldInterval, oldWidth := isattyStdout, hbAfter, hbInterval, consoleWidth
	isattyStdout = func() bool { return true }
	consoleWidth = func() int { return 40 } // 40 列：逼出裁剪逻辑
	hbAfter = 100 * time.Millisecond
	hbInterval = 50 * time.Millisecond
	defer func() {
		isattyStdout, hbAfter, hbInterval, consoleWidth = oldIsatty, oldAfter, oldInterval, oldWidth
	}()

	var out string
	done := make(chan struct{})
	go func() {
		out = captureStdout(t, func() {
			stop := startHeartbeat("2023年中华人民共和国海关进出口税则.pdf", false)
			time.Sleep(400 * time.Millisecond)
			stop()
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("心跳未按时结束")
	}

	if !strings.Contains(out, "仍在处理") {
		t.Fatalf("未产生心跳输出: %q", out)
	}
	for _, frame := range strings.Split(out, "\r") {
		if w := displayWidth(frame); w > 39 {
			t.Errorf("提示帧宽度 %d 超过可用 39 列: %q", w, frame)
		}
	}
}
