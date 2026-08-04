//go:build !unix || ios

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
