//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	procGetConsoleProcessList      = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	procGetConsoleMode             = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleMode")
	procSetConsoleMode             = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleScreenBufferInfo")
)

// CONSOLE_SCREEN_BUFFER_INFO 及其成员类型，与 Windows API 定义一一对应。
// 全部为定长整数、无指针字段，可安全地通过 syscall 传址调用。
type coord struct{ x, y int16 }

type smallRect struct{ left, top, right, bottom int16 }

type consoleScreenBufferInfo struct {
	size              coord
	cursorPosition    coord
	attributes        uint16
	window            smallRect
	maximumWindowSize coord
}

// 控制台模式位
const (
	enableQuickEditMode = 0x0040 // 快速编辑（鼠标框选）
	enableExtendedFlags = 0x0080 // 与快速编辑配套，必须同时置位才生效
)

func init() {
	launchedByDoubleClick = detectDoubleClick
	isattyStdout = stdoutIsConsole
	// 心跳提示要按窗口实际宽度裁剪。Win7 默认仅 80 列，
	// 提示一旦超宽就会折行，每刷新一次都会多出一行空白。
	consoleWidth = winConsoleWidth
	// 拖放/双击启动的控制台窗口，是我们自己创建的交互窗口，最容易误触快速编辑：
	// 用户把文件拖进来之后鼠标就停在窗口里，一次单击就会让控制台进入
	// “标记（Mark）”状态。此时控制台会**阻塞一切标准输出写入**，
	// 表现为「处理到一半突然长时间没有任何输出」，敲回车或 ESC 才恢复。
	// 这类批量工具的输出不需要鼠标框选，直接关掉快速编辑模式。
	if launchedByDoubleClick() {
		disableQuickEdit()
	}
}

// detectDoubleClick 用「当前控制台内挂载的进程数」判断启动方式：
//   - 双击图标 / 把文件拖到图标上：Windows 会为本进程新建一个控制台，
//     其中只有本进程，GetConsoleProcessList 返回 1；
//   - 从 cmd / PowerShell / 终端启动：控制台由父 shell 持有，返回 >= 2；
//   - 输出被重定向且没有控制台：返回 0。
//
// 仅当返回 1（独占控制台）时才认为需要暂停等待用户看清结果。
func detectDoubleClick() bool {
	var pids [16]uint32
	n, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	return n == 1
}

// stdoutIsConsole 判断标准输出是否为交互式控制台：
// GetConsoleMode 只对控制台句柄成功，对文件/管道句柄返回失败。
func stdoutIsConsole() bool {
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil || h == 0 || h == syscall.InvalidHandle {
		return false
	}
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}

// winConsoleWidth 返回当前控制台每行可显示的列数；不是控制台（输出被重定向）
// 或调用失败时返回 0，由调用方按 80 列兜底。
//
// 取「窗口可见宽度」与「缓冲区宽度」的较小值：Win7 下缓冲区可能比窗口宽
// （窗口出现横向滚动条），此时文本是在缓冲区宽度处折行的。
func winConsoleWidth() int {
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil || h == 0 || h == syscall.InvalidHandle {
		return 0
	}
	var info consoleScreenBufferInfo
	if r, _, _ := procGetConsoleScreenBufferInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0
	}
	w := int(info.window.right-info.window.left) + 1
	if bw := int(info.size.x); bw > 0 && bw < w {
		w = bw
	}
	if w <= 0 {
		return 0
	}
	return w
}

// disableQuickEdit 关闭当前控制台的「快速编辑模式」。
//
// ⚠️ 必须用 **标准输入句柄** 设置：ENABLE_QUICK_EDIT_MODE(0x0040) 是
// **控制台输入模式**（由 ReadConsoleInput 等的缓冲区行为决定），
// 对 STD_OUTPUT_HANDLE 调用 SetConsoleMode 不会生效。
// v2.3.0 曾误用输出句柄，导致 Win7 拖放批处理仍会「处理到一半永久冻结」。
//
// 失败时静默返回：这只是体验优化，任何一步拿不到句柄都不应影响主流程。
// 注意：关闭后仍可通过控制台窗口标题栏 →「编辑」→「标记」手动选取文本，
// 只是不会因为一次无意的点击就把程序“冻住”。
func disableQuickEdit() {
	// 首选输入句柄（正确目标）
	if h, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE); err == nil {
		if disableQuickEditOn(h) {
			return
		}
	}
	// 兜底：极少数环境下输入句柄不可用（如 stdin 被重定向），再试输出句柄。
	// 对输出句柄设置输入模式通常无效，但试一下没有副作用。
	if h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE); err == nil {
		disableQuickEditOn(h)
	}
}

// disableQuickEditOn 在指定控制台句柄上清除快速编辑位。
// 返回是否成功读取并写回了控制台模式。
func disableQuickEditOn(h syscall.Handle) bool {
	if h == 0 || h == syscall.InvalidHandle {
		return false
	}
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false // 不是控制台（已被重定向）
	}
	newMode := (mode &^ uint32(enableQuickEditMode)) | uint32(enableExtendedFlags)
	if newMode == mode {
		return true // 已是关闭状态
	}
	r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(newMode))
	return r != 0
}
