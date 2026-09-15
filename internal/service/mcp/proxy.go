package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zcray0326/mcp-gateway/internal/model"
	"github.com/zcray0326/mcp-gateway/internal/telemetry"
	"github.com/zcray0326/mcp-gateway/pkg/apierrors"
	"github.com/zcray0326/mcp-gateway/pkg/types"
)

func authorizeProxyServerAccess(ctx context.Context, serverName string) error {
	// Dev 模式面向本机个人使用，不做客户端级授权；Enterprise 模式才校验 allow-list。
	serverMode := ctx.Value("mode").(model.ServerMode)
	if !model.IsEnterpriseMode(serverMode) {
		return nil
	}

	c := ctx.Value("client").(*model.McpClient)
	if !c.CheckHasServerAccess(serverName) {
		return fmt.Errorf("client %s is not authorized to access MCP server %s", c.Name, serverName)
	}

	return nil
}

// MCPProxyToolCallHandler 是客户端调用工具时的核心转发入口：
// server__tool 解析 -> 权限校验 -> 获取上游连接 -> 改回原始工具名 -> 调用上游 -> 记录指标。
func (m *MCPService) MCPProxyToolCallHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	started := time.Now()
	outcome := telemetry.ToolCallOutcomeSuccess

	name := request.Params.Name
	serverName, toolName, ok := splitServerToolName(name)
	if !ok {
		return nil, fmt.Errorf("tool name does not contain a %s separator: %w", serverToolNameSep, apierrors.ErrInvalidInput)
	}

	if err := authorizeProxyServerAccess(ctx, serverName); err != nil {
		return nil, err
	}

	// defer 保证成功和失败路径都记录一次指标；outcome 在错误分支中更新。
	defer func() {
		m.metrics.RecordToolCall(ctx, serverName, toolName, outcome, time.Since(started))
	}()

	// 工具的规范名只携带 Server 名称，完整连接配置仍以数据库记录为准。
	server, err := m.GetMcpServer(serverName)
	if err != nil {
		// TODO: 后续应区分“Server 不存在”和数据库故障，让错误类型及指标更准确。
		outcome = telemetry.ToolCallOutcomeError

		return nil, fmt.Errorf(
			"failed to get details about MCP server %s from DB: %w", serverName, err,
		)
	}

	session, err := m.getSession(ctx, server)
	if err != nil {
		outcome = telemetry.ToolCallOutcomeError
		return nil, err
	}
	defer session.closeIfApplicable()

	// server__tool 是网关为防止重名添加的前缀，上游只认识它原本的 tool 名。
	request.Params.Name = toolName
	// 不透传客户端 Header：上游鉴权由网关自己的 Server 配置负责，透传可能泄漏客户端 Token。
	// 相关背景：https://github.com/mcpjungle/MCPJungle/issues/252
	request.Header = nil

	res, err := session.client.CallTool(ctx, request)
	if err != nil {
		outcome = telemetry.ToolCallOutcomeError
		session.invalidateOnError(err) // Stateful 连接出错后立即淘汰，避免后续请求反复命中坏连接。
	}

	// MCP 结果保持原结构返回，网关不解释具体工具的业务内容。
	return res, err
}

// mcpProxyResourceHandler 将网关 URI 映射回上游原始 URI，再读取并把结果 URI 重写成网关 URI。
// 这样客户端始终面对统一命名空间，不需要知道资源实际来自哪个上游。
func (m *MCPService) mcpProxyResourceHandler(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	// 数据库同时保存网关 URI、原始 URI 和所属 Server，是资源路由表。
	resource, err := m.GetResource(request.Params.URI)
	if err != nil {
		return nil, fmt.Errorf("failed to get resource %s from DB: %w", request.Params.URI, err)
	}

	if err := authorizeProxyServerAccess(ctx, resource.Server.Name); err != nil {
		return nil, err
	}

	session, err := m.getSession(ctx, &resource.Server)
	if err != nil {
		return nil, err
	}
	defer session.closeIfApplicable()

	request.Params.URI = resource.OriginalURI

	// 与 Tool 调用一致，丢弃下游 Header，改用网关保存的上游鉴权信息。
	request.Header = nil

	res, err := session.client.ReadResource(ctx, request)
	if err != nil {
		session.invalidateOnError(err)
		return nil, err
	}

	return rewriteResourceContentsURI(res.Contents, resource.URI), nil
}

// mcpProxyPromptHandler 与工具转发流程相同，只是调用的 MCP 能力换成 prompts/get。
func (m *MCPService) mcpProxyPromptHandler(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	started := time.Now()
	outcome := telemetry.PromptCallOutcomeSuccess

	name := request.Params.Name
	serverName, promptName, ok := splitServerPromptName(name)
	if !ok {
		return nil, fmt.Errorf("prompt name does not contain a %s separator: %w", serverPromptNameSep, apierrors.ErrInvalidInput)
	}

	if err := authorizeProxyServerAccess(ctx, serverName); err != nil {
		return nil, err
	}

	// 统一在函数返回时记录调用结果和端到端耗时。
	defer func() {
		m.metrics.RecordPromptCall(ctx, serverName, promptName, outcome, time.Since(started))
	}()

	// 根据规范名中的 Server 前缀加载上游配置。
	server, err := m.GetMcpServer(serverName)
	if err != nil {
		// TODO: 后续应细分记录不存在与数据库故障，避免都归类为同一种内部错误。
		outcome = telemetry.PromptCallOutcomeError

		return nil, fmt.Errorf(
			"failed to get details about MCP server %s from DB: %w", serverName, err,
		)
	}

	session, err := m.getSession(ctx, server)
	if err != nil {
		outcome = telemetry.PromptCallOutcomeError
		return nil, err
	}
	defer session.closeIfApplicable()

	// 去掉网关添加的 server__ 前缀，再把原始 Prompt 名发送给上游。
	request.Params.Name = promptName
	// 防止把下游客户端凭据误发给上游。
	request.Header = nil

	// 网关只负责路由，不修改上游返回的 Prompt 内容。
	res, err := session.client.GetPrompt(ctx, request)
	if err != nil {
		outcome = telemetry.PromptCallOutcomeError
		session.invalidateOnError(err) // 连接异常时淘汰 Stateful 会话，等待下次调用重建。
	}

	return res, err
}

// initMCPProxyServer 在进程启动时用数据库重建内存中的 MCP 能力目录。
// 只加载 Enabled 数据，并按上游 Transport 分配到常规代理或兼容 SSE 的代理。
func (m *MCPService) initMCPProxyServer() error {
	mcpServerModelsCache := make(map[string]*model.McpServer)
	// 第一阶段：恢复 Tools，同时建立 Tool 实例索引，供调用和 Tool Group 使用。
	tools, err := m.ListTools()
	if err != nil {
		return fmt.Errorf("failed to list tools from DB: %w", err)
	}

	for _, tm := range tools {
		if !tm.Enabled {
			// Disabled 记录保留在数据库中，但不能被客户端发现和调用。
			continue
		}

		// 数据库存的是可持久化模型，mcp-go 代理需要运行时 Tool 对象。
		tool, err := convertToolModelToMcpObject(&tm)
		if err != nil {
			return fmt.Errorf("failed to convert tool model to MCP object for tool %s: %w", tm.Name, err)
		}

		// 同一 Server 往往包含多个 Tool，缓存 Server 模型可避免循环内重复查库。
		var server *model.McpServer
		serverName, _, _ := splitServerToolName(tool.Name)

		server, exists := mcpServerModelsCache[serverName]
		if !exists {
			server, err = m.GetMcpServer(serverName)
			if err != nil {
				return fmt.Errorf(
					"init mcp proxy server: failed to get MCP server %s for tool %s from DB: %w", serverName, tool.Name, err,
				)
			}
			// 后续 Prompts 也复用这份缓存。
			mcpServerModelsCache[serverName] = server
		}

		if server.Transport == types.TransportSSE {
			m.sseMcpProxyServer.AddTool(tool, m.MCPProxyToolCallHandler)
		} else {
			m.mcpProxyServer.AddTool(tool, m.MCPProxyToolCallHandler)
		}

		m.addToolInstance(tool)
	}

	// 第二阶段：恢复 Prompts，命名和 Transport 分流规则与 Tools 相同。
	prompts, err := m.ListPrompts()
	if err != nil {
		return fmt.Errorf("failed to list prompts from DB: %w", err)
	}

	for _, pm := range prompts {
		if !pm.Enabled {
			// 禁用的 Prompt 不进入客户端可发现目录。
			continue
		}

		// 将持久化模型转换成 mcp-go 的运行时对象。
		prompt, err := convertPromptModelToMcpObject(&pm)
		if err != nil {
			return fmt.Errorf("failed to convert prompt model to MCP object for prompt %s: %w", pm.Name, err)
		}

		// 通过所属 Server 的 Transport 决定注册到哪个代理实例。
		var server *model.McpServer
		serverName, _, _ := splitServerPromptName(prompt.Name)

		server, exists := mcpServerModelsCache[serverName]
		if !exists {
			server, err = m.GetMcpServer(serverName)
			if err != nil {
				return fmt.Errorf(
					"init mcp proxy server: failed to get MCP server %s for tool %s from DB: %w", serverName, prompt.Name, err,
				)
			}
			mcpServerModelsCache[serverName] = server
		}

		if server.Transport == types.TransportSSE {
			m.sseMcpProxyServer.AddPrompt(prompt, m.mcpProxyPromptHandler)
		} else {
			m.mcpProxyServer.AddPrompt(prompt, m.mcpProxyPromptHandler)
		}
	}

	// 第三阶段：恢复 Resources。
	resources, err := m.ListResources()
	if err != nil {
		return fmt.Errorf("failed to list resources from DB: %w", err)
	}

	for _, rm := range resources {
		if !rm.Enabled {
			continue
		}

		// Resource 查询已预加载关联的 Server，因此不需要再次使用 Server 缓存查库。

		resource, err := convertResourceModelToMcpObject(&rm)
		if err != nil {
			return fmt.Errorf("failed to convert resource model to MCP object for resource %s: %w", rm.URI, err)
		}
		resource.Name = rm.Name

		if rm.Server.Transport == types.TransportSSE {
			m.sseMcpProxyServer.AddResource(resource, m.mcpProxyResourceHandler)
		} else {
			m.mcpProxyServer.AddResource(resource, m.mcpProxyResourceHandler)
		}
	}

	return nil
}
