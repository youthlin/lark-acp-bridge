//go:build unix

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	modeRun     = "run"
	modeStart   = "start"
	modeStop    = "stop"
	modeRestart = "restart"

	daemonEnvToken = "LARK_ACP_BRIDGE_DAEMON=1"
)

// waitForDeadline 在超时前以 interval 为间隔反复调用 cond，直到 cond 返回 true。
// 用于 start/stop 这类一次性 CLI 命令等待后台进程就绪/退出。
// 运行中的 daemon 服务本身不使用轮询。
func waitForDeadline(deadline time.Time, interval time.Duration, cond func() bool) bool {
	if cond() {
		return true
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if cond() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
	return false
}

func runMode(args []string) string {
	if len(args) == 0 {
		return modeRestart
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
	return daemonChild
}

func runDaemon(mode, configPath string) error {
	switch mode {
	case modeStart:
		return startDaemon(configPath)
	case modeStop:
		return stopDaemon(configPath)
	case modeRestart:
		return restartDaemon(configPath)
	default:
		return fmt.Errorf("不支持的运行模式: %s", mode)
	}
}

func startDaemon(configPath string) error {
	if err := ensureRuntimeDir(configPath); err != nil {
		return err
	}
	unlock, err := acquireInstanceLock(configPath)
	if err != nil {
		return err
	}
	unlock()

	pidFile := daemonPIDFile(configPath)
	if pid, running, err := readRunningPID(pidFile); err != nil {
		return err
	} else if running {
		return fmt.Errorf("服务已在后台运行, pid=%d, pidfile=%s", pid, pidFile)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取可执行文件路径: %w", err)
	}
	logFile := daemonLogFile(configPath)
	logf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开日志文件 %s: %w", logFile, err)
	}
	defer logf.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", os.DevNull, err)
	}
	defer devNull.Close()

	cmd := exec.Command(exe, childArgs(os.Args[1:])...)
	cmd.Dir = mustGetwd()
	cmd.Env = append(os.Environ(), daemonEnvToken)
	cmd.Stdin = devNull
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动后台子进程: %w", err)
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(8 * time.Second)
	if waitForDeadline(deadline, 250*time.Millisecond, func() bool {
		_, running, err := readRunningPID(pidFile)
		return err == nil && running
	}) {
		pid, _, _ := readRunningPID(pidFile)
		fmt.Printf("后台启动成功, pidfile: %s(%d), log: %s\n", pidFile, pid, logFile)
		return nil
	}
	return fmt.Errorf("子进程未在预期时间内写入 pid 文件, 请查看日志: %s", logFile)
}

func stopDaemon(configPath string) error {
	pidFile := daemonPIDFile(configPath)
	pid, running, err := readRunningPID(pidFile)
	if err != nil {
		return err
	}
	if !running {
		fmt.Printf("服务未运行, pidfile: %s\n", pidFile)
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("查找进程 %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("停止进程 %d: %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	if waitForDeadline(deadline, 200*time.Millisecond, func() bool {
		return !processExists(pid)
	}) {
		_ = os.Remove(pidFile)
		fmt.Printf("服务已停止, pid=%d\n", pid)
		return nil
	}
	return fmt.Errorf("已发送停止信号, 但进程仍未退出, pid=%d", pid)
}

func restartDaemon(configPath string) error {
	pidFile := daemonPIDFile(configPath)
	if pid, running, err := readRunningPID(pidFile); err != nil {
		return err
	} else if running {
		fmt.Printf("检测到后台服务运行中, 准备重启, pid=%d\n", pid)
		if err := stopDaemon(configPath); err != nil {
			return err
		}
	} else {
		fmt.Printf("服务当前未运行, 直接启动\n")
	}
	return startDaemon(configPath)
}

func writeDaemonPIDFile(configPath string) (func(), error) {
	if !isDaemonChild() {
		return func() {}, nil
	}
	if err := ensureRuntimeDir(configPath); err != nil {
		return nil, err
	}
	pidFile := daemonPIDFile(configPath)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, fmt.Errorf("写入 pid 文件 %s: %w", pidFile, err)
	}
	return func() {
		_ = os.Remove(pidFile)
	}, nil
}

func childArgs(args []string) []string {
	out := make([]string, 0, len(os.Args))
	out = append(out, "--daemon-child")
	skipNext := false
	for _, arg := range args {
		if skipNext {
			out = append(out, arg)
			skipNext = false
			continue
		}
		switch arg {
		case modeRun, modeStart, modeStop, modeRestart:
			continue
		default:
			out = append(out, arg)
			if arg == "-config" || arg == "--config" {
				skipNext = true
			}
		}
	}
	return out
}

func ensureRuntimeDir(configPath string) error {
	return os.MkdirAll(filepath.Dir(configPath), 0o755)
}

func daemonPIDFile(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "lark-acp-bridge.pid")
}

func daemonLogFile(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "lark-acp-bridge.log")
}

func readRunningPID(pidFile string) (pid int, running bool, err error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		_ = os.Remove(pidFile)
		return 0, false, nil
	}
	if !processExists(pid) || !processLooksLikeSelf(pid) {
		_ = os.Remove(pidFile)
		return pid, false, nil
	}
	return pid, true, nil
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func processLooksLikeSelf(pid int) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return false
	}
	if !bytes.Contains(data, []byte(daemonEnvToken)) {
		return false
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	return bytes.Contains(cmdline, []byte("-daemon-child"))
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
