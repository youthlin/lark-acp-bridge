package main

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

const (
	serviceUnitName    = "lark-acp-bridge.service"
	launchdServiceName = "com.youthlin.lark-acp-bridge"
)

var currentExecutable = os.Executable

type serviceInstallOptions struct {
	GOOS       string
	ConfigPath string
	BinaryPath string
	WorkingDir string
	Path       string
}

type serviceInstallResult struct {
	RestartCommand []string
}

func runServiceCommand(configPath string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: lark-acp-bridge service <install|uninstall>")
	}
	switch args[0] {
	case "install":
		return runServiceInstall(configPath, args[1:])
	case "uninstall", "remove", "rm":
		return runServiceUninstall(args[1:])
	default:
		return fmt.Errorf("未知 service 子命令: %s", args[0])
	}
}

func runServiceInstall(configPath string, args []string) error {
	fs := flagSet("service install")
	binaryPath := fs.String("binary", "", "bridge 可执行文件路径（默认：当前可执行文件）")
	workingDir := fs.String("working-dir", "", "服务工作目录（默认：用户主目录）")
	servicePath := fs.String("path", "", "服务进程 PATH（默认：当前 PATH 去除临时目录后的稳定部分）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("用法: lark-acp-bridge service install [--binary <path>] [--working-dir <dir>] [--path <PATH>]")
	}
	options := serviceInstallOptions{
		GOOS:       runtime.GOOS,
		ConfigPath: configPath,
		BinaryPath: *binaryPath,
		WorkingDir: *workingDir,
		Path:       *servicePath,
	}
	return installService(options)
}

func runServiceUninstall(args []string) error {
	fs := flagSet("service uninstall")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("用法: lark-acp-bridge service uninstall")
	}
	return uninstallService(runtime.GOOS)
}

func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func installService(options serviceInstallOptions) error {
	goos := strings.TrimSpace(options.GOOS)
	if goos == "" {
		goos = runtime.GOOS
	}
	configPath, err := configPathForService(options.ConfigPath)
	if err != nil {
		return err
	}
	binaryPath, err := binaryPathForService(options.BinaryPath)
	if err != nil {
		return err
	}
	workingDir, err := workingDirForService(options.WorkingDir)
	if err != nil {
		return err
	}
	servicePath := servicePathForInstall(goos, options.Path)
	goCache := serviceGoCacheForInstall()
	target, err := serviceTargetPath(goos)
	if err != nil {
		return err
	}
	var content string
	var result serviceInstallResult
	switch goos {
	case "linux":
		content = renderSystemdUserService(binaryPath, configPath, workingDir, servicePath, goCache)
		result.RestartCommand = systemdRestartCommand()
	case "darwin":
		content = renderLaunchdAgent(binaryPath, configPath, workingDir, serviceLogFile(configPath), servicePath, goCache)
		result.RestartCommand = launchdRestartCommand(os.Getuid())
	default:
		return unsupportedServicePlatform(goos)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("创建服务目录 %s: %w", filepath.Dir(target), err)
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入服务文件 %s: %w", target, err)
	}
	if err := updateRestartCommand(configPath, result.RestartCommand); err != nil {
		return err
	}
	printServiceInstallNextSteps(goos, target, configPath, result)
	return nil
}

func uninstallService(goos string) error {
	target, err := serviceTargetPath(goos)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("服务文件不存在: %s\n", target)
			return nil
		}
		return fmt.Errorf("删除服务文件 %s: %w", target, err)
	}
	printServiceUninstallNextSteps(goos, target)
	return nil
}

func serviceTargetPath(goos string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("查找用户主目录: %w", err)
	}
	switch goos {
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", serviceUnitName), nil
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", launchdServiceName+".plist"), nil
	default:
		return "", unsupportedServicePlatform(goos)
	}
}

func serviceLogFile(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "lark-acp-bridge.log")
}

func unsupportedServicePlatform(goos string) error {
	return fmt.Errorf("当前平台 %q 暂不支持 service install，只支持 Linux systemd user service 和 macOS launchd user agent", goos)
}

func configPathForService(path string) (string, error) {
	path, err := configPathOrDefault(path)
	if err != nil {
		return "", err
	}
	return absolutePath(path)
}

func binaryPathForService(path string) (string, error) {
	fromCurrentExecutable := strings.TrimSpace(path) == ""
	if fromCurrentExecutable {
		exe, err := currentExecutable()
		if err != nil {
			return "", fmt.Errorf("获取当前可执行文件路径: %w", err)
		}
		path = exe
	} else {
		var err error
		path, err = config.ExpandPath(path)
		if err != nil {
			return "", err
		}
	}
	path, err := absolutePath(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if fromCurrentExecutable && looksLikeGoRunExecutable(path) {
		return "", fmt.Errorf("当前可执行文件像 go run 临时文件，请先 go install ./cmd/lark-acp-bridge 后运行已安装的 lark-acp-bridge，或用 --binary 指定稳定可执行文件路径")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("检查可执行文件 %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("可执行文件路径是目录: %s", path)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("可执行文件不可执行: %s", path)
	}
	return path, nil
}

func workingDirForService(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("查找用户主目录: %w", err)
		}
		path = home
	} else {
		var err error
		path, err = config.ExpandPath(path)
		if err != nil {
			return "", err
		}
	}
	path, err := absolutePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("检查工作目录 %s: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("工作目录不是目录: %s", path)
	}
	return path, nil
}

func absolutePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("转换绝对路径 %s: %w", path, err)
	}
	return filepath.Clean(abs), nil
}

func looksLikeGoRunExecutable(path string) bool {
	path = filepath.ToSlash(path)
	return strings.Contains(path, "/go-build") && strings.Contains(path, "/exe/")
}

func servicePathForInstall(goos, path string) string {
	if strings.TrimSpace(path) == "" {
		path = os.Getenv("PATH")
	}
	if normalized := normalizeServicePath(path); normalized != "" {
		return normalized
	}
	return defaultServicePath(goos)
}

func normalizeServicePath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	parts := filepath.SplitList(path)
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || transientServicePathPart(part) {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return strings.Join(out, string(os.PathListSeparator))
}

func transientServicePathPart(path string) bool {
	path = filepath.ToSlash(path)
	return strings.Contains(path, "/.trae/tmp/") ||
		strings.Contains(path, "/go-build")
}

func defaultServicePath(goos string) string {
	switch goos {
	case "linux":
		return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	case "darwin":
		return "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	default:
		return ""
	}
}

func serviceGoCacheForInstall() string {
	return serviceGoCacheForUID(os.Getuid())
}

func serviceGoCacheForUID(uid int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("lark-acp-bridge-%d", uid), "go-build")
}

func renderSystemdUserService(binaryPath, configPath, workingDir, servicePath, goCache string) string {
	execStart := systemdCommand([]string{binaryPath, "--config", configPath, "run"})
	var environment strings.Builder
	if strings.TrimSpace(servicePath) != "" {
		fmt.Fprintf(&environment, "Environment=%s\n", systemdQuote("PATH="+servicePath))
	}
	if strings.TrimSpace(goCache) != "" {
		fmt.Fprintf(&environment, "Environment=%s\n", systemdQuote("GOCACHE="+goCache))
	}
	return fmt.Sprintf(`[Unit]
Description=Lark ACP Bridge
After=network.target

[Service]
Type=simple
WorkingDirectory=%s
%sExecStart=%s
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=default.target
`, systemdQuote(workingDir), environment.String(), execStart)
}

func systemdCommand(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, systemdQuote(arg))
	}
	return strings.Join(parts, " ")
}

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	if value == "" {
		return `""`
	}
	needsQuote := false
	for _, r := range value {
		if unicode.IsSpace(r) || r == '"' || r == '\\' || r == '\'' || r == ';' {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return value
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func renderLaunchdAgent(binaryPath, configPath, workingDir, logPath, servicePath, goCache string) string {
	args := []string{binaryPath, "--config", configPath, "run"}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>`)
	b.WriteString(xmlText(launchdServiceName))
	b.WriteString(`</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, arg := range args {
		b.WriteString("    <string>")
		b.WriteString(xmlText(arg))
		b.WriteString("</string>\n")
	}
	b.WriteString(`  </array>
  <key>WorkingDirectory</key>
  <string>`)
	b.WriteString(xmlText(workingDir))
	b.WriteString(`</string>
`)
	if strings.TrimSpace(servicePath) != "" || strings.TrimSpace(goCache) != "" {
		b.WriteString(`  <key>EnvironmentVariables</key>
  <dict>
`)
		if strings.TrimSpace(servicePath) != "" {
			b.WriteString(`    <key>PATH</key>
    <string>`)
			b.WriteString(xmlText(servicePath))
			b.WriteString(`</string>
`)
		}
		if strings.TrimSpace(goCache) != "" {
			b.WriteString(`    <key>GOCACHE</key>
    <string>`)
			b.WriteString(xmlText(goCache))
			b.WriteString(`</string>
`)
		}
		b.WriteString(`  </dict>
`)
	}
	b.WriteString(`  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>`)
	b.WriteString(xmlText(logPath))
	b.WriteString(`</string>
  <key>StandardErrorPath</key>
  <string>`)
	b.WriteString(xmlText(logPath))
	b.WriteString(`</string>
</dict>
</plist>
`)
	return b.String()
}

func updateRestartCommand(configPath string, command []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("读取配置文件以更新 restart_command: %w", err)
	}
	cfg.RestartCommand = append([]string(nil), command...)
	if err := config.Write(configPath, cfg); err != nil {
		return fmt.Errorf("写入配置文件以更新 restart_command: %w", err)
	}
	return nil
}

func systemdRestartCommand() []string {
	return []string{"systemctl", "--user", "restart", serviceUnitName}
}

func launchdRestartCommand(uid int) []string {
	return []string{"launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, launchdServiceName)}
}

func xmlText(value string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

func printServiceInstallNextSteps(goos, target, configPath string, result serviceInstallResult) {
	fmt.Printf("已写入服务文件: %s\n", target)
	fmt.Printf("已更新配置 restart_command: %s\n", strings.Join(result.RestartCommand, " "))
	switch goos {
	case "linux":
		fmt.Println("启用并启动:")
		fmt.Printf("  systemctl --user daemon-reload\n")
		fmt.Printf("  systemctl --user enable --now %s\n", serviceUnitName)
		fmt.Println("如果需要退出登录后继续运行，可执行:")
		fmt.Println("  loginctl enable-linger $USER")
	case "darwin":
		uid := os.Getuid()
		fmt.Println("启用并启动:")
		fmt.Printf("  launchctl bootstrap gui/%d %s\n", uid, target)
		fmt.Printf("  launchctl enable gui/%d/%s\n", uid, launchdServiceName)
		fmt.Printf("  launchctl kickstart -k gui/%d/%s\n", uid, launchdServiceName)
	}
	fmt.Printf("当前服务配置文件参数: %s\n", configPath)
}

func printServiceUninstallNextSteps(goos, target string) {
	fmt.Printf("已删除服务文件: %s\n", target)
	switch goos {
	case "linux":
		fmt.Println("如服务已启用，可执行:")
		fmt.Printf("  systemctl --user disable --now %s\n", serviceUnitName)
		fmt.Println("  systemctl --user daemon-reload")
	case "darwin":
		uid := os.Getuid()
		fmt.Println("如服务已加载，可执行:")
		fmt.Printf("  launchctl bootout gui/%d/%s\n", uid, launchdServiceName)
	}
}
