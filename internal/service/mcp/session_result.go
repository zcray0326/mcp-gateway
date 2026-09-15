package mcp

import (
	"context"
	"errors"
	"io"
	"strings"
	"syscall"

	"github.com/mark3labs/mcp-go/client"
	"github.com/zcray0326/mcp-gateway/internal/model"
	"github.com/zcray0326/mcp-gateway/pkg/types"
)

// connectionErrorPatterns 汇总常见连接错误文本，用于判断 Stateful 会话是否已经不可复用。
// 这是对不同 Transport/操作系统错误包装差异的兜底，下面还会结合标准错误类型判断。
var connectionErrorPatterns = []string{
	"connection refused",
	"connection reset",
	"connection closed",
	"broken pipe",
	"eof",
	"no such host",
	"network is unreachable",
	"timeout",
	"context canceled",
	"context deadline exceeded",
	"transport",
	"dial",
	"i/o timeout",
	"use of closed network connection",
}

// sessionResult 抹平 Stateful 与 Stateless 的生命周期差异。
// 调用方只需 defer closeIfApplicable，无需关心当前 Client 是复用连接还是一次性连接。
type sessionResult struct {
	client      *client.Client
	shouldClose bool // Stateless 为 true；Stateful 的连接由 SessionManager 统一关闭。

	// Stateful 调用失败时需要这两个字段定位并淘汰缓存连接。
	serverName     string
	sessionManager *SessionManager
}

// closeIfApplicable 只关闭本次调用新建的 Stateless 连接，不影响复用的 Stateful 会话。
func (sr *sessionResult) closeIfApplicable() {
	if sr.shouldClose && sr.client != nil {
		sr.client.Close()
	}
}

// invalidateOnError 仅在确认属于连接故障时淘汰 Stateful 会话。
// 工具自身返回业务错误不代表连接损坏，不应因此制造无意义的重连。
func (sr *sessionResult) invalidateOnError(err error) {
	if err == nil || sr.shouldClose || sr.sessionManager == nil {
		return // Stateless 本来就会关闭，或当前没有需要处理的错误。
	}

	// 下次请求会自动创建新连接，这里不做同步重试以避免产生重复副作用。
	if isConnectionError(err) {
		sr.sessionManager.InvalidateSession(sr.serverName, err.Error())
	}
}

// getSession 是两种会话模式的统一入口：Stateful 从管理器复用，Stateless 每次新建并在调用后关闭。
func (m *MCPService) getSession(ctx context.Context, server *model.McpServer) (*sessionResult, error) {
	if server.SessionMode == types.SessionModeStateful {
		// Stateful 适合依赖冷启动状态或连续会话的上游。
		mcpClient, err := m.sessionManager.GetOrCreateSession(ctx, server)
		if err != nil {
			return nil, err
		}
		return &sessionResult{
			client:         mcpClient,
			shouldClose:    false, // 本次请求结束后仍保留连接。
			serverName:     server.Name,
			sessionManager: m.sessionManager,
		}, nil
	}

	// Stateless 是默认模式：隔离请求状态，代价是每次都要完成建连和初始化。
	mcpClient, err := createMcpServerConnectionWithDB(ctx, m.db, server, m.mcpServerInitReqTimeoutSec, true)
	if err != nil {
		return nil, err
	}
	return &sessionResult{
		client:      mcpClient,
		shouldClose: true, // 由调用方 defer 在请求结束时释放。
	}, nil
}

// isConnectionError 同时检查错误文本和 errors.Is 错误链，兼容不同 Transport 的错误包装方式。
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())

	for _, pattern := range connectionErrorPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	// 标准错误类型比文本匹配更可靠，优先覆盖常见网络和上下文终止场景。
	if errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}

	return false
}
