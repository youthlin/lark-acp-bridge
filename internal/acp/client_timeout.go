package acp

import "time"

// 非 prompt ACP RPC 的默认超时。
// 当调用方传入的 ctx 没有 deadline 时，call 层按 method 套用这些默认值，
// 避免 agent 子进程在握手/会话操作阶段卡死导致请求永久挂起。
// prompt 自身由上层 runtime 套 idle/max deadline，不在此列。
//
// 定义为 var 而非 const，仅为允许单元测试临时改小以快速验证超时行为；
// 生产代码不应修改。
var (
	// initialize/authenticate/logout 是握手与鉴权，agent 冷启动可能较慢，给较长超时。
	defaultInitializeTimeout = 60 * time.Second
	defaultAuthTimeout       = 60 * time.Second
	// session/new|load|resume 涉及 agent 恢复上下文，给较长超时。
	defaultSessionStartTimeout = 120 * time.Second
	// 关闭/删除/列举/配置/模式切换通常很快。
	defaultSessionOpTimeout = 30 * time.Second
)

// defaultRPCTimeout 返回指定 method 在 ctx 无 deadline 时应套用的默认超时。
// prompt 以及未知 method 返回 0，表示不套用默认超时（完全由调用方 ctx 控制）。
func defaultRPCTimeout(method string) time.Duration {
	switch method {
	case "initialize":
		return defaultInitializeTimeout
	case "authenticate", "logout":
		return defaultAuthTimeout
	case "session/new", "session/load", "session/resume":
		return defaultSessionStartTimeout
	case "session/close", "session/delete", "session/list",
		"session/set_config_option", "session/set_mode":
		return defaultSessionOpTimeout
	default:
		return 0
	}
}
