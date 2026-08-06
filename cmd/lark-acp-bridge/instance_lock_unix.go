//go:build unix && !ios

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func acquireInstanceLock(configPath string) (func(), error) {
	lockFile, err := instanceLockFile(configPath)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开实例锁文件 %s: %w", lockFile, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, instanceAlreadyRunningError(configPath, lockFile)
		}
		return nil, fmt.Errorf("获取实例锁 %s: %w", lockFile, err)
	}
	if err := writeInstanceLockPID(file); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
