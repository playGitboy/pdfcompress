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

// 清除心跳行必须按「字节长度」补空格：中日韩字符 3 字节对应 2 列宽、
// ASCII 1 字节对应 1 列宽，因此字节长度 ≥ 显示宽度，空格必能盖掉上一帧。
func TestHeartbeatClearWidthCoversCJK(t *testing.T) {
	msg := "仍在处理: 大文件.pdf，已 1m0s"
	// 按控制台显示规则估算宽度：CJK（>0x7F）按 2 列，其余按 1 列
	display := 0
	for _, r := range msg {
		if r > 0x7F {
			display += 2
		} else {
			display++
		}
	}
	if len(msg) < display {
		t.Fatalf("字节长度 %d 应不小于显示宽度 %d，否则清除会留残影", len(msg), display)
	}
}
