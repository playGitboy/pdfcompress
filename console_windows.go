//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	procGetConsoleProcessList = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	procGetConsoleMode        = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleMode")
	procSetConsoleMode        = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")
)

// 控制台模式位
const (
	enableQuickEditMode = 0x0040 // 快速编辑（鼠标框选）
	enableExtendedFlags = 0x0080 // 与快速编辑配套，必须同时置位才生效
)

func init() {
	launchedByDoubleClick = detectDoubleClick
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

// disableQuickEdit 关闭当前控制台的「快速编辑模式」。
//
// 失败时静默返回：这只是体验优化，任何一步拿不到句柄都不应影响主流程。
// 注意：关闭后仍可通过控制台窗口标题栏 →「编辑」→「标记」手动选取文本，
// 只是不会因为一次无意的点击就把程序“冻住”。
func disableQuickEdit() {
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil || h == 0 || h == syscall.InvalidHandle {
		return
	}
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return // 不是控制台（已被重定向）
	}
	newMode := (mode &^ uint32(enableQuickEditMode)) | uint32(enableExtendedFlags)
	if newMode == mode {
		return
	}
	procSetConsoleMode.Call(uintptr(h), uintptr(newMode))
}
