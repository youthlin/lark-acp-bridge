//go:build !unix

// 本项目不考虑 ios 平台 所以构建标签里不额外考虑ios
// 不然 gopls 会自动创建跨平台构建视图 导致编辑器会提示
// ios/arm64 requires external (cgo) linking, but cgo is not enabled [ios,arm64]

package main

import "fmt"

const (
	modeRun     = "run"
	modeStart   = "start"
	modeStop    = "stop"
	modeRestart = "restart"
)

func runMode(args []string) string {
	if len(args) == 0 {
		return modeRun
	}
	return args[0]
}

func isRunMode(command string) bool {
	switch command {
	case modeRun, modeStart, modeStop, modeRestart:
		return true
	default:
		return false
	}
}

func isDaemonChild() bool {
	return false
}

func runDaemon(mode, configPath string) error {
	return fmt.Errorf("当前平台不支持后台运行模式 %q，请使用 run 前台运行", mode)
}

func writeDaemonPIDFile(configPath string) (func(), error) {
	return func() {}, nil
}
