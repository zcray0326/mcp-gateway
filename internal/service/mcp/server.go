package mcp

import (
	"context"
	"errors"
	"fmt"
	"log"

	mcpgotransport "github.com/mark3labs/mcp-go/client/transport"
	"github.com/zcray0326/mcp-gateway/internal/model"
	"github.com/zcray0326/mcp-gateway/pkg/apierrors"
	"github.com/zcray0326/mcp-gateway/pkg/types"
	"gorm.io/gorm"
)

// RegisterMcpServerWithOAuthSupport 先按普通方式连接上游；只有收到 Unauthorized 时才进入 OAuth 流程。
// 这样无鉴权、Bearer Token 和 OAuth 上游可以共用一个注册入口。
func (m *MCPService) RegisterMcpServerWithOAuthSupport(
	ctx context.Context,
	input *types.RegisterServerInput,
	s *model.McpServer,
	force bool,
	initiatedBy string,
) error {
	// 第一次连接不携带历史 OAuth 凭据，用它探测上游是否真的要求 OAuth。
	err := m.registerMcpServerWithoutOAuth(ctx, s)
	if err == nil {
		return nil
	}

	// STDIO 不存在远程 OAuth；HTTP/SSE 的非 401 错误也应原样返回，不能误导用户授权。
	if s.Transport != types.TransportStreamableHTTP && s.Transport != types.TransportSSE {
		return err
	}
	if !errors.Is(err, mcpgotransport.ErrUnauthorized) {
		return err
	}

	// 确认为上游 OAuth 挑战后，保存待完成会话并把授权信息返回调用方。
	return m.bootstrapUpstreamOAuth(ctx, input, s, force, initiatedBy)
}

// registerMcpServerWithoutOAuth 执行用于探测认证要求的首次注册。
func (m *MCPService) registerMcpServerWithoutOAuth(ctx context.Context, s *model.McpServer) error {
	return m.registerMcpServer(ctx, s, false)
}

// finalizeMcpServerRegistration 在 OAuth 回调完成后携带已保存凭据，再次执行正式注册。
func (m *MCPService) finalizeMcpServerRegistration(ctx context.Context, s *model.McpServer) error {
	return m.registerMcpServer(ctx, s, true)
}

// registerMcpServer 是上游注册的核心流程：校验配置、完成 MCP 握手、保存 Server、发现并登记能力。
// Tools 是网关的核心能力，发现失败会让整个注册失败；Prompts 和 Resources 采用尽力而为策略。
// 成功登记的能力会同时进入数据库和内存代理，立即对客户端可见。
func (m *MCPService) registerMcpServer(ctx context.Context, s *model.McpServer, useStoredUpstreamAuth bool) error {
	if err := validateServerName(s.Name); err != nil {
		return err
	}

	// 新注册 Server 默认可用，管理员之后可整体停用。
	s.Enabled = true

	// 只有网络 Transport 才校验 URL；STDIO 使用 command/args，不应套用 URL 规则。
	switch s.Transport {
	case types.TransportStreamableHTTP:
		conf, err := s.GetStreamableHTTPConfig()
		if err != nil {
			return err
		}
		if err := validateURL(conf.URL); err != nil {
			return err
		}
	case types.TransportSSE:
		conf, err := s.GetSSEConfig()
		if err != nil {
			return err
		}
		if err := validateURL(conf.URL); err != nil {
			return err
		}
	}

	mcpClient, err := createMcpServerConnectionWithDB(
		ctx,
		m.db,
		s,
		m.mcpServerInitReqTimeoutSec,
		useStoredUpstreamAuth,
	)
	if err != nil {
		return err
	}
	defer mcpClient.Close()

	// 先保存 Server，后续 Tool/Prompt/Resource 记录才能通过外键关联它。
	if err := m.db.Create(s).Error; err != nil {
		return fmt.Errorf("failed to register mcp server: %w", err)
	}

	if err = m.registerServerTools(ctx, s, mcpClient); err != nil {
		return fmt.Errorf("failed to register tools for MCP server %s: %w", s.Name, err)
	}

	// 上游明确声明能力后才发起发现；Prompts/Resources 失败只告警，不回滚已注册的 Tools。
	if mcpClient.GetServerCapabilities().Prompts != nil {
		if err = m.registerServerPrompts(ctx, s, mcpClient); err != nil {
			log.Printf("[WARN] failed to register prompts for MCP server %s: %v", s.Name, err)
		}
	}
	if mcpClient.GetServerCapabilities().Resources != nil {
		if err = m.registerServerResources(ctx, s, mcpClient); err != nil {
			log.Printf("[WARN] failed to register resources for MCP server %s: %v", s.Name, err)
		}
	}

	return nil
}

// DeregisterMcpServer 按“能力 -> Server -> OAuth 状态 -> Stateful 连接”的顺序彻底注销上游。
// 先移除能力可以保证代理目录不会留下指向已删除 Server 的悬空入口。
func (m *MCPService) DeregisterMcpServer(name string) error {
	s, err := m.GetMcpServer(name)
	if err != nil {
		return fmt.Errorf("failed to get MCP server %s from DB: %w", name, err)
	}
	if err := m.deregisterServerTools(s); err != nil {
		return fmt.Errorf(
			"failed to deregister tools for server %s, cannot proceed with server deregistration: %w",
			name,
			err,
		)
	}
	if err := m.deregisterServerPrompts(s); err != nil {
		return fmt.Errorf(
			"failed to deregister prompts for server %s, cannot proceed with server deregistration: %w",
			name,
			err,
		)
	}
	if err := m.deregisterServerResources(s); err != nil {
		return fmt.Errorf(
			"failed to deregister resources for server %s, cannot proceed with server deregistration: %w",
			name,
			err,
		)
	}
	if err := m.db.Unscoped().Delete(s).Error; err != nil {
		return fmt.Errorf("failed to deregister server %s: %w", name, err)
	}
	if err := m.db.Unscoped().Where("server_name = ?", name).Delete(&model.UpstreamOAuthToken{}).Error; err != nil {
		return fmt.Errorf("failed to remove upstream OAuth tokens for server %s: %w", name, err)
	}
	if err := m.db.Unscoped().Where("server_name = ?", name).Delete(&model.UpstreamOAuthPendingSession{}).Error; err != nil {
		return fmt.Errorf("failed to remove pending upstream OAuth sessions for server %s: %w", name, err)
	}

	// 最后关闭可能仍被缓存的 Stateful 连接。
	m.sessionManager.CloseSession(name)

	return nil
}

// ListMcpServers 返回注册表中的全部上游 Server。
func (m *MCPService) ListMcpServers() ([]model.McpServer, error) {
	var servers []model.McpServer
	if err := m.db.Find(&servers).Error; err != nil {
		return nil, err
	}
	return servers, nil
}

// GetMcpServer 按唯一名称读取上游，并把 GORM 的未找到错误转换成 API 层可识别的领域错误。
func (m *MCPService) GetMcpServer(name string) (*model.McpServer, error) {
	var serverModel model.McpServer
	if err := m.db.Where("name = ?", name).First(&serverModel).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("MCP server %s not found: %w", name, apierrors.ErrNotFound)
		}
		return nil, err
	}
	return &serverModel, nil
}

// EnableMcpServer 启用 Server 本身，并级联恢复它的 Tools、Prompts 和 Resources 到代理目录。
func (m *MCPService) EnableMcpServer(name string) ([]string, []string, error) {
	if err := validateServerName(name); err != nil {
		return nil, nil, err
	}
	if err := m.setMcpServerEnabled(name, true); err != nil {
		return nil, nil, fmt.Errorf("failed to mark server %s enabled: %w", name, err)
	}
	toolsEnabled, err := m.EnableTools(name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to enable tools for server %s: %w", name, err)
	}
	promptsEnabled, err := m.EnablePrompts(name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to enable prompts for server %s: %w", name, err)
	}
	if _, err := m.EnableResources(name); err != nil {
		return nil, nil, fmt.Errorf("failed to enable resources for server %s: %w", name, err)
	}
	return toolsEnabled, promptsEnabled, nil
}

// DisableMcpServer 停用 Server 及其全部能力，使客户端无法再发现或调用它们，但保留数据库配置。
func (m *MCPService) DisableMcpServer(name string) ([]string, []string, error) {
	if err := validateServerName(name); err != nil {
		return nil, nil, err
	}
	if err := m.setMcpServerEnabled(name, false); err != nil {
		return nil, nil, fmt.Errorf("failed to mark server %s disabled: %w", name, err)
	}
	toolsDisabled, err := m.DisableTools(name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to disable tools for server %s: %w", name, err)
	}
	promptsDisabled, err := m.DisablePrompts(name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to disable prompts for server %s: %w", name, err)
	}
	if _, err := m.DisableResources(name); err != nil {
		return nil, nil, fmt.Errorf("failed to disable resources for server %s: %w", name, err)
	}
	return toolsDisabled, promptsDisabled, nil
}

// SetDashboardServerEnabled 复用标准启停流程，确保 Dashboard 与 CLI 的级联语义完全一致。
func (m *MCPService) SetDashboardServerEnabled(name string, enabled bool) error {
	if enabled {
		_, _, err := m.EnableMcpServer(name)
		return err
	}
	_, _, err := m.DisableMcpServer(name)
	return err
}

// setMcpServerEnabled 只更新 Server 的持久化标志；具体能力的运行时增删由上层启停流程完成。
func (m *MCPService) setMcpServerEnabled(name string, enabled bool) error {
	server, err := m.GetMcpServer(name)
	if err != nil {
		return err
	}
	if server.Enabled == enabled {
		return nil
	}
	server.Enabled = enabled
	if err := m.db.Save(server).Error; err != nil {
		return fmt.Errorf("failed to set server %s enabled=%t: %w", name, enabled, err)
	}
	return nil
}
