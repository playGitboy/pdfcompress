//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var procGetConsoleProcessList = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")

func init() {
	launchedByDoubleClick = detectDoubleClick
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
