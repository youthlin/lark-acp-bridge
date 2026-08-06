//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const windowsSharingViolation syscall.Errno = 32

func acquireInstanceLock(configPath string) (func(), error) {
	lockFile, err := instanceLockFile(configPath)
	if err != nil {
		return nil, err
	}
	path, err := syscall.UTF16PtrFromString(lockFile)
	if err != nil {
		return nil, fmt.Errorf("转换实例锁文件路径 %s: %w", lockFile, err)
	}
	handle, err := syscall.CreateFile(
		path,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, windowsSharingViolation) {
			return nil, instanceAlreadyRunningError(configPath, lockFile)
		}
		return nil, fmt.Errorf("获取实例锁 %s: %w", lockFile, err)
	}
	file := os.NewFile(uintptr(handle), lockFile)
	if file == nil {
		_ = syscall.CloseHandle(handle)
		return nil, fmt.Errorf("创建实例锁文件句柄失败: %s", lockFile)
	}
	if err := writeInstanceLockPID(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = file.Close()
	}, nil
}
