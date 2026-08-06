package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func instanceLockFile(configPath string) (string, error) {
	canonicalPath, err := canonicalConfigPath(configPath)
	if err != nil {
		return "", err
	}
	return canonicalPath + ".lock", nil
}

func canonicalConfigPath(configPath string) (string, error) {
	absolutePath, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf("解析配置文件绝对路径 %s: %w", configPath, err)
	}
	canonicalPath, err := filepath.EvalSymlinks(absolutePath)
	if err != nil {
		return "", fmt.Errorf("解析配置文件真实路径 %s: %w", absolutePath, err)
	}
	return canonicalPath, nil
}

func instanceAlreadyRunningError(configPath, lockFile string) error {
	pidText := ""
	if data, err := os.ReadFile(lockFile); err == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil && pid > 0 {
			pidText = fmt.Sprintf(", pid=%d", pid)
		}
	}
	return fmt.Errorf("同一配置的 Bridge 实例已在运行%s, config=%s, lock=%s", pidText, configPath, lockFile)
}

func writeInstanceLockPID(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("清空实例锁文件 %s: %w", file.Name(), err)
	}
	if _, err := file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		return fmt.Errorf("写入实例锁文件 %s: %w", file.Name(), err)
	}
	return nil
}
