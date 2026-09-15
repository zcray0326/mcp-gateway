package mcp

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/zcray0326/mcp-gateway/internal/model"
	"github.com/zcray0326/mcp-gateway/pkg/types"
	"gorm.io/gorm"
)

const (
	// DefaultSessionIdleTimeoutSec 是 Stateful 会话默认空闲时间，超时后主动释放上游连接。
	DefaultSessionIdleTimeoutSec = 3600 // 1 hour

	// sessionCleanupIntervalSec 是后台扫描周期。它不是会话超时时间，只决定清理动作多久执行一次。
	sessionCleanupIntervalSec = 60 // 1 minute
)

// ManagedSession 包装一个可复用的上游连接，并记录创建和最近使用时间，供空闲回收判断。
type ManagedSession struct {
	ServerName string
	Client     *client.Client
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// SessionManager 按 ServerName 缓存 Stateful 连接。
// 同一上游只保留一个连接，互斥锁同时保护 map 和 LastUsedAt，避免并发请求重复创建或边用边删。
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*ManagedSession // key 为 ServerName

	idleTimeoutSec    int
	initReqTimeoutSec int
	cleanupTicker     *time.Ticker
	cleanupStopChan   chan struct{}
	createSessionFunc func(ctx context.Context, s *model.McpServer, initReqTimeoutSec int) (*client.Client, error)
}

// SessionManagerConfig 描述会话管理器的依赖与生命周期参数。
type SessionManagerConfig struct {
	// DB 用于建连时读取已经持久化的上游 OAuth 凭据。
	DB *gorm.DB

	// IdleTimeoutSec 为 0 表示不因空闲而回收；负数回退到默认值。
	IdleTimeoutSec int

	// InitReqTimeoutSec 限制与上游进行 MCP initialize 握手的等待时间。
	InitReqTimeoutSec int
}

// NewSessionManager 创建管理器；启用空闲超时后会同时启动一个后台清理协程。
func NewSessionManager(cfg *SessionManagerConfig) *SessionManager {
	idleTimeout := cfg.IdleTimeoutSec
	if idleTimeout < 0 {
		idleTimeout = DefaultSessionIdleTimeoutSec
	}

	sm := &SessionManager{
		sessions:          make(map[string]*ManagedSession),
		idleTimeoutSec:    idleTimeout,
		initReqTimeoutSec: cfg.InitReqTimeoutSec,
		cleanupStopChan:   make(chan struct{}),
		createSessionFunc: func(ctx context.Context, s *model.McpServer, initReqTimeoutSec int) (*client.Client, error) {
			return createMcpServerConnectionWithDB(ctx, cfg.DB, s, initReqTimeoutSec, true)
		},
	}

	// 只有配置了超时才启动 ticker，避免无意义的常驻协程。
	if idleTimeout > 0 {
		sm.startCleanupRoutine()
	}

	return sm
}

// GetOrCreateSession 实现 Stateful 连接的“存在则复用，不存在则创建”。
// 整个检查和创建过程持有写锁，以防两个并发请求为同一上游各建一条连接。
func (sm *SessionManager) GetOrCreateSession(ctx context.Context, server *model.McpServer) (*client.Client, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 命中缓存时刷新最近使用时间，延后空闲回收。
	if session, exists := sm.sessions[server.Name]; exists {
		session.LastUsedAt = time.Now()
		return session.Client, nil
	}

	// 未命中时根据上游 Transport 创建 HTTP、SSE 或 STDIO 客户端。
	mcpClient, err := sm.createSessionFunc(ctx, server, sm.initReqTimeoutSec)
	if err != nil {
		return nil, fmt.Errorf("failed to create session for server '%s': %w", server.Name, err)
	}

	sm.sessions[server.Name] = &ManagedSession{
		ServerName: server.Name,
		Client:     mcpClient,
		CreatedAt:  time.Now(),
		LastUsedAt: time.Now(),
	}

	log.Printf("[SessionManager] Created new stateful session for server '%s'", server.Name)

	return mcpClient, nil
}

// CloseSession 主动关闭并移除指定上游的连接，常用于注销或停用 Server。
func (sm *SessionManager) CloseSession(serverName string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if session, exists := sm.sessions[serverName]; exists {
		if session.Client != nil {
			if err := session.Client.Close(); err != nil {
				log.Printf("[SessionManager] Error closing session for server '%s': %v", serverName, err)
			}
		}
		delete(sm.sessions, serverName)
		log.Printf("[SessionManager] Closed session for server '%s'", serverName)
	}
}

// InvalidateSession 在调用上游发生连接类错误时淘汰坏连接。
// 它不立即重试；下一次请求会通过 GetOrCreateSession 自动创建新连接。
func (sm *SessionManager) InvalidateSession(serverName string, reason string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if session, exists := sm.sessions[serverName]; exists {
		if session.Client != nil {
			if err := session.Client.Close(); err != nil {
				log.Printf("[SessionManager] Error closing unhealthy session for server '%s': %v", serverName, err)
			}
		}
		delete(sm.sessions, serverName)
		log.Printf("[SessionManager] Invalidated unhealthy session for server '%s': %s", serverName, reason)
	}
}

// CloseAllSessions 关闭当前管理的全部连接，供服务优雅退出使用。
func (sm *SessionManager) CloseAllSessions() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for name, session := range sm.sessions {
		if session.Client != nil {
			if err := session.Client.Close(); err != nil {
				log.Printf("[SessionManager] Error closing session for server '%s': %v", name, err)
			}
		}
		delete(sm.sessions, name)
	}

	log.Printf("[SessionManager] Closed all sessions")
}

// Shutdown 先通知清理协程退出，再关闭所有连接。
func (sm *SessionManager) Shutdown() {
	// 仅在 ticker 存在时关闭 channel，避免重复关闭未启用的清理流程。
	if sm.cleanupTicker != nil {
		close(sm.cleanupStopChan)
	}

	// 统一释放仍存活的上游连接。
	sm.CloseAllSessions()
}

// HasSession 判断指定上游当前是否有缓存连接。
func (sm *SessionManager) HasSession(serverName string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	_, exists := sm.sessions[serverName]
	return exists
}

// SessionCount 返回当前活跃的 Stateful 连接数。
func (sm *SessionManager) SessionCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

// startCleanupRoutine 周期性扫描空闲连接，并通过 stop channel 响应服务关闭。
func (sm *SessionManager) startCleanupRoutine() {
	sm.cleanupTicker = time.NewTicker(time.Duration(sessionCleanupIntervalSec) * time.Second)

	go func() {
		for {
			select {
			case <-sm.cleanupTicker.C:
				sm.cleanupIdleSessions()
			case <-sm.cleanupStopChan:
				sm.cleanupTicker.Stop()
				return
			}
		}
	}()
}

// cleanupIdleSessions 关闭超过阈值未被使用的连接。扫描期间持有写锁，防止连接被同时复用。
func (sm *SessionManager) cleanupIdleSessions() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.idleTimeoutSec == 0 {
		return // 0 表示明确关闭空闲回收。
	}

	now := time.Now()
	idleThreshold := time.Duration(sm.idleTimeoutSec) * time.Second

	for name, session := range sm.sessions {
		if now.Sub(session.LastUsedAt) > idleThreshold {
			log.Printf("[SessionManager] Closing idle session for server '%s' (idle for %v)", name, now.Sub(session.LastUsedAt))
			if session.Client != nil {
				if err := session.Client.Close(); err != nil {
					log.Printf("[SessionManager] Error closing session for server '%s': %v", name, err)
				}
			}
			delete(sm.sessions, name)
		}
	}
}

func createMcpServerConnectionWithDB(
	ctx context.Context,
	db *gorm.DB,
	s *model.McpServer,
	initReqTimeoutSec int,
	useStoredUpstreamAuth bool,
) (*client.Client, error) {
	// Transport 决定底层如何建连，但对 SessionManager 上层统一暴露为 mcp-go Client。
	switch s.Transport {
	case types.TransportStreamableHTTP:
		return createHTTPMcpServerConn(ctx, db, s, initReqTimeoutSec, useStoredUpstreamAuth)
	case types.TransportSSE:
		return createSSEMcpServerConn(ctx, db, s, useStoredUpstreamAuth)
	case types.TransportStdio:
		return runStdioServer(ctx, s, initReqTimeoutSec)
	default:
		return nil, fmt.Errorf("unsupported transport type: %s", s.Transport)
	}
}
