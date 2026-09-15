// Package mcp 是网关的核心业务层，负责把数据库中的注册信息、对外 MCP 代理和上游 MCP Server 串联起来。
package mcp

import (
	"fmt"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zcray0326/mcp-gateway/internal/telemetry"
	"gorm.io/gorm"
)

// ServiceConfig 集中保存 MCPService 的依赖，便于启动阶段一次性完成装配，也方便测试时替换其中的实现。
type ServiceConfig struct {
	DB *gorm.DB

	McpProxyServer    *server.MCPServer
	SseMcpProxyServer *server.MCPServer

	Metrics telemetry.CustomMetrics

	McpServerInitReqTimeout int

	// SessionManager 只管理 Stateful 上游的长连接；不传时由服务创建默认实现。
	SessionManager *SessionManager
}

// MCPService 是 MCP 领域的总控服务。
// 管理接口通过它修改数据库和内存代理，客户端的 MCP 请求也通过它转发到真正的上游 Server。
// 因此注册、启停等操作必须同时维护“持久化状态”和“运行时状态”的一致性。
type MCPService struct {
	db *gorm.DB

	mcpProxyServer    *server.MCPServer
	sseMcpProxyServer *server.MCPServer

	// toolInstances 保存已经暴露给客户端的工具对象。key 使用 server__tool 形式，避免不同上游工具重名。
	toolInstances map[string]mcp.Tool
	mu            sync.RWMutex

	// 工具启停后通过回调通知 Tool Group 等依赖方同步更新自己的运行时视图。
	toolDeletionCallback ToolDeletionCallback
	toolAdditionCallback ToolAdditionCallback

	metrics telemetry.CustomMetrics

	mcpServerInitReqTimeoutSec int

	// sessionManager 复用 Stateful 上游连接；Stateless 请求不会长期占用这里的连接。
	sessionManager *SessionManager
}

// NewMCPService 创建核心服务，并把数据库中已启用的 Tools、Prompts、Resources 恢复到内存代理。
// 这一步相当于运行时状态重建，因此失败时服务不应继续启动。
func NewMCPService(c *ServiceConfig) (*MCPService, error) {
	if c == nil {
		return nil, fmt.Errorf("service config is nil")
	}
	if c.DB == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	if c.McpProxyServer == nil || c.SseMcpProxyServer == nil {
		return nil, fmt.Errorf("mcp proxy servers must not be nil")
	}

	// 允许外部注入 SessionManager；正常启动时未注入就使用默认配置创建。
	sessionManager := c.SessionManager
	if sessionManager == nil {
		sessionManager = NewSessionManager(&SessionManagerConfig{
			DB:                c.DB,
			IdleTimeoutSec:    DefaultSessionIdleTimeoutSec,
			InitReqTimeoutSec: c.McpServerInitReqTimeout,
		})
	}

	s := &MCPService{
		db: c.DB,

		mcpProxyServer:    c.McpProxyServer,
		sseMcpProxyServer: c.SseMcpProxyServer,

		toolInstances: make(map[string]mcp.Tool),
		mu:            sync.RWMutex{},

		// 默认使用空回调，调用方无需反复判断 callback 是否为 nil。
		toolDeletionCallback: func(toolNames ...string) {},
		toolAdditionCallback: func(toolName string) error { return nil },

		metrics: c.Metrics,

		mcpServerInitReqTimeoutSec: c.McpServerInitReqTimeout,

		sessionManager: sessionManager,
	}
	if err := s.initMCPProxyServer(); err != nil {
		return nil, fmt.Errorf("failed to initialize MCP proxy server: %w", err)
	}
	return s, nil
}

// Shutdown 关闭所有 Stateful 上游连接，避免网关退出时遗留子进程或网络连接。
func (m *MCPService) Shutdown() {
	if m.sessionManager != nil {
		m.sessionManager.Shutdown()
	}
}
