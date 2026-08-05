package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

var ErrServerOutputClosed = errors.New("ACP server 输出已关闭")

// Client owns one local ACP child process and multiplexes JSON-RPC calls,
// notifications, prompt updates, and server-initiated permission requests.
type Client struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	workspace string
	cwd       string

	nextID atomic.Int64

	writeMu  sync.Mutex
	promptMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan rpcResponse

	agentRequestMu      sync.Mutex
	agentRequestCancels map[string]context.CancelFunc

	capMu      sync.RWMutex
	initialize InitializeResult

	toolMu    sync.RWMutex
	toolCalls map[string]map[string]ToolCallInfo

	updateMu       sync.Mutex
	updateHandlers map[int64]UpdateHandler
	nextHandlerID  atomic.Int64

	permissionMu     sync.RWMutex
	permissionScopes map[string]permissionScope
	nextPromptGen    atomic.Int64

	closeOnce sync.Once
}

func Start(ctx context.Context, agent config.AgentConfig, workspace string) (*Client, error) {
	if agent.Command == "" {
		return nil, fmt.Errorf("agent 启动命令为空")
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(agent.Command, agent.Args...)
	cmd.Env = os.Environ()
	for key, value := range agent.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("打开 stdin 管道: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("打开 stdout 管道: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 ACP server: %w", err)
	}

	client := &Client{
		cmd:                 cmd,
		stdin:               stdin,
		workspace:           workspace,
		pending:             make(map[string]chan rpcResponse),
		agentRequestCancels: make(map[string]context.CancelFunc),
		toolCalls:           make(map[string]map[string]ToolCallInfo),
		permissionScopes:    make(map[string]permissionScope),
		updateHandlers:      make(map[int64]UpdateHandler),
	}
	client.nextID.Store(1)
	go client.readLoop(stdout)
	return client, nil
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
			_, _ = c.cmd.Process.Wait()
		}
	})
	return nil
}

func (c *Client) ensureInitialized() error {
	c.capMu.RLock()
	initialized := c.initialize.ProtocolVersion != 0
	c.capMu.RUnlock()
	if !initialized {
		return fmt.Errorf("ACP client 尚未 initialize")
	}
	return nil
}
