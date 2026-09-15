package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zcray0326/mcp-gateway/internal/model"
	"github.com/zcray0326/mcp-gateway/internal/telemetry"
	"github.com/zcray0326/mcp-gateway/pkg/apierrors"
	"github.com/zcray0326/mcp-gateway/pkg/types"
	"gorm.io/gorm"
)

// ToolDeletionCallback 在工具被注销或停用后触发，用于让 Tool Group 等派生视图同步移除工具。
type ToolDeletionCallback func(toolNames ...string)

// ToolAdditionCallback 在工具注册或重新启用后触发，使依赖方可以同步新增工具。
type ToolAdditionCallback func(toolName string) error

// ListTools 返回全部工具，并把数据库中的原始名称转换成 server__tool 规范名。
// 例如 git Server 的 commit Tool 对外叫 git__commit，从而解决不同上游工具重名问题。
func (m *MCPService) ListTools() ([]model.Tool, error) {
	var tools []model.Tool
	if err := m.db.Find(&tools).Error; err != nil {
		return nil, err
	}
	// 数据库按 ServerID 关联工具，对外展示时补上可读的 Server 名称。
	for i := range tools {
		var s model.McpServer
		if err := m.db.First(&s, "id = ?", tools[i].ServerID).Error; err != nil {
			return nil, fmt.Errorf("failed to get server for tool %s: %w", tools[i].Name, err)
		}
		tools[i].Name = mergeServerToolNames(s.Name, tools[i].Name)
	}
	return tools, nil
}

// ListToolsByServer 查询指定上游的工具，并统一返回规范名。
func (m *MCPService) ListToolsByServer(name string) ([]model.Tool, error) {
	if err := validateServerName(name); err != nil {
		return nil, err
	}

	s, err := m.GetMcpServer(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get MCP server %s from DB: %w", name, err)
	}

	var tools []model.Tool
	if err := m.db.Where("server_id = ?", s.ID).Find(&tools).Error; err != nil {
		return nil, fmt.Errorf("failed to get tools for server %s from DB: %w", name, err)
	}

	// 即使限定了 Server，返回给调用方时仍保持全局统一的命名格式。
	for i := range tools {
		tools[i].Name = mergeServerToolNames(s.Name, tools[i].Name)
	}

	return tools, nil
}

func (m *MCPService) GetTool(name string) (*model.Tool, error) {
	serverName, toolName, ok := splitServerToolName(name)
	if !ok {
		return nil, fmt.Errorf("tool name does not contain a %s separator: %w", serverToolNameSep, apierrors.ErrInvalidInput)
	}

	s, err := m.GetMcpServer(serverName)
	if err != nil {
		return nil, fmt.Errorf("failed to get MCP server %s from DB: %w", serverName, err)
	}

	var tool model.Tool
	if err := m.db.Where("server_id = ? AND name = ?", s.ID, toolName).First(&tool).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("tool %s not found: %w", name, apierrors.ErrNotFound)
		}
		return nil, fmt.Errorf("failed to get tool %s from DB: %w", name, err)
	}
	// 查询数据库时使用原始名，返回 API 前恢复成规范名。
	tool.Name = name
	return &tool, nil
}

// GetToolInstance 从内存索引读取运行时 Tool，读锁允许多个请求并发查询。
func (m *MCPService) GetToolInstance(name string) (mcp.Tool, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tool, exists := m.toolInstances[name]
	return tool, exists
}

// GetToolParentServer 从 server__tool 规范名反查提供该工具的上游 Server。
func (m *MCPService) GetToolParentServer(name string) (*model.McpServer, error) {
	serverName, _, ok := splitServerToolName(name)
	if !ok {
		return nil, fmt.Errorf("tool name does not contain a %s separator: %w", serverToolNameSep, apierrors.ErrInvalidInput)
	}
	return m.GetMcpServer(serverName)
}

// InvokeTool 是 REST API 使用的工具调用入口。
// 它与 MCP 代理入口最终都调用上游 Client，但这里还要把 SDK 响应转换为 REST 返回结构。
func (m *MCPService) InvokeTool(ctx context.Context, name string, args map[string]any) (*types.ToolInvokeResult, error) {
	started := time.Now()
	outcome := telemetry.ToolCallOutcomeError

	serverName, toolName, ok := splitServerToolName(name)
	if !ok {
		return nil, fmt.Errorf("tool name does not contain a %s separator: %w", serverToolNameSep, apierrors.ErrInvalidInput)
	}

	// 默认按失败处理，只有响应转换也成功后才把 outcome 改为 success。
	defer func() {
		m.metrics.RecordToolCall(ctx, serverName, toolName, outcome, time.Since(started))
	}()

	serverModel, err := m.GetMcpServer(serverName)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to get details about MCP server %s from DB: %w",
			serverName,
			err,
		)
	}

	session, err := m.getSession(ctx, serverModel)
	if err != nil {
		return nil, err
	}
	defer session.closeIfApplicable()

	callToolReq := mcp.CallToolRequest{}
	callToolReq.Params.Name = toolName
	callToolReq.Params.Arguments = args

	callToolResp, err := session.client.CallTool(ctx, callToolReq)
	if err != nil {
		session.invalidateOnError(err) // Stateful 连接失败后淘汰，避免复用已损坏的会话。
		return nil, fmt.Errorf("failed to call tool %s on MCP server %s: %w", toolName, serverName, err)
	}

	// MCP Content 本身是多态结构；统一转成 map 后，REST 层无需依赖 SDK 的具体内容类型。
	result, err := m.convertToolCallResToAPIRes(callToolResp)
	if err != nil {
		return nil, fmt.Errorf("failed to convert MCP response to api response: %w", err)
	}

	outcome = telemetry.ToolCallOutcomeSuccess

	return result, nil
}

// SetToolDeletionCallback 注册工具删除通知。目前用于维护 Tool Group 的运行时工具集合。
func (m *MCPService) SetToolDeletionCallback(callback ToolDeletionCallback) {
	m.toolDeletionCallback = callback
}

// SetToolAdditionCallback 注册工具新增通知。
func (m *MCPService) SetToolAdditionCallback(callback ToolAdditionCallback) {
	m.toolAdditionCallback = callback
}

// EnableTools 接受两种 entity：server__tool 表示单个工具，server 表示该上游的全部工具。
func (m *MCPService) EnableTools(entity string) ([]string, error) {
	return m.setToolsEnabled(entity, true)
}

// DisableTools 与 EnableTools 使用相同参数规则，只是把目标从代理可发现目录中移除。
func (m *MCPService) DisableTools(entity string) ([]string, error) {
	return m.setToolsEnabled(entity, false)
}

// setToolsEnabled 同时维护三份状态：数据库 Enabled、mcp-go 代理目录、toolInstances 内存索引。
// 任意一份漏更新都会出现“数据库显示启用但客户端发现不到”等不一致问题。
func (m *MCPService) setToolsEnabled(entity string, enabled bool) ([]string, error) {
	serverName, toolName, ok := splitServerToolName(entity)
	if ok {
		// 能拆成 server__tool，说明这是单工具操作。
		s, err := m.GetMcpServer(serverName)
		if err != nil {
			return nil, fmt.Errorf("failed to get MCP server %s: %w", serverName, err)
		}

		var tool model.Tool
		if err := m.db.Where("server_id = ? AND name = ?", s.ID, toolName).First(&tool).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, fmt.Errorf("tool %s not found: %w", entity, apierrors.ErrNotFound)
			}
			return nil, fmt.Errorf("failed to get tool %s: %w", entity, err)
		}

		if tool.Enabled == enabled {
			return []string{entity}, nil // 已是目标状态，按幂等操作直接成功。
		}

		tool.Enabled = enabled
		if err := m.db.Save(&tool).Error; err != nil {
			return nil, fmt.Errorf("failed to set tool %s enabled=%t: %w", entity, enabled, err)
		}

		if enabled {
			// 启用时从数据库模型还原 mcp-go Tool，并按 Transport 放回对应代理。
			mcpTool, err := convertToolModelToMcpObject(&tool)
			if err != nil {
				return nil, fmt.Errorf("failed to convert tool model to MCP object for tool %s: %w", tool.Name, err)
			}
			// 代理目录使用全局唯一的规范名。
			mcpTool.Name = entity

			if s.Transport == types.TransportSSE {
				m.sseMcpProxyServer.AddTool(mcpTool, m.MCPProxyToolCallHandler)
			} else {
				m.mcpProxyServer.AddTool(mcpTool, m.MCPProxyToolCallHandler)
			}

			// Tool Group 等功能通过内存索引取得完整 Tool 定义。
			m.addToolInstance(mcpTool)
			// 通知依赖方同步恢复该工具。
			m.notifyToolAddition(mcpTool.Name)
		} else {
			// 停用时从对应 Transport 的代理目录移除。
			if s.Transport == types.TransportSSE {
				m.sseMcpProxyServer.DeleteTools(entity)
			} else {
				m.mcpProxyServer.DeleteTools(entity)
			}

			// 同时清理内存索引和派生视图。
			m.deleteToolInstances(entity)
			// 通知 Tool Group 等依赖方。
			m.notifyToolDeletion(entity)
		}

		return []string{entity}, nil
	}

	// 无分隔符时把 entity 解释成 Server 名，对其全部工具批量操作。
	s, err := m.GetMcpServer(entity)
	if err != nil {
		return nil, fmt.Errorf("failed to get MCP server %s: %w", serverName, err)
	}

	var tools []model.Tool
	if err := m.db.Where("server_id = ?", s.ID).Find(&tools).Error; err != nil {
		return nil, fmt.Errorf("failed to get tools for server %s: %w", entity, err)
	}

	var changedToolNames []string
	for i := range tools {
		if tools[i].Enabled == enabled {
			continue // 已符合目标状态，不重复写库或触发通知。
		}
		tools[i].Enabled = enabled
		if err := m.db.Save(&tools[i]).Error; err != nil {
			return nil, fmt.Errorf("failed to set tool %s enabled=%t: %w", tools[i].Name, enabled, err)
		}
		canonicalToolName := mergeServerToolNames(s.Name, tools[i].Name)

		if enabled {
			mcpTool, err := convertToolModelToMcpObject(&tools[i])
			if err != nil {
				return nil, fmt.Errorf("failed to convert tool model to MCP object for tool %s: %w", tools[i].Name, err)
			}
			// 批量分支同样必须使用规范名注册到代理。
			mcpTool.Name = canonicalToolName

			if s.Transport == types.TransportSSE {
				m.sseMcpProxyServer.AddTool(mcpTool, m.MCPProxyToolCallHandler)
			} else {
				m.mcpProxyServer.AddTool(mcpTool, m.MCPProxyToolCallHandler)
			}

			m.addToolInstance(mcpTool)
			m.notifyToolAddition(mcpTool.Name)
		} else {
			if s.Transport == types.TransportSSE {
				m.sseMcpProxyServer.DeleteTools(canonicalToolName)
			} else {
				m.mcpProxyServer.DeleteTools(canonicalToolName)
			}

			m.deleteToolInstances(canonicalToolName)
			m.notifyToolDeletion(canonicalToolName)
		}

		changedToolNames = append(changedToolNames, canonicalToolName)
	}

	return changedToolNames, nil
}

// registerServerTools 在注册上游时执行 tools/list，并把发现结果写入数据库和运行时代理。
func (m *MCPService) registerServerTools(ctx context.Context, s *model.McpServer, c *client.Client) error {
	// MCP 初始化完成后调用 tools/list 获取上游当前声明的全部工具。
	resp, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("failed to fetch tools from MCP server %s: %w", s.Name, err)
	}
	for _, tool := range resp.Tools {
		canonicalToolName := mergeServerToolNames(s.Name, tool.GetName())

		// Schema 和 Annotations 用 JSON 保存，序列化失败不应阻断其他工具注册。
		jsonSchema, _ := json.Marshal(tool.InputSchema)

		// Annotations 同样采用尽力而为策略。
		annotationsJSON, _ := json.Marshal(tool.Annotations)

		t := &model.Tool{
			ServerID:    s.ID,
			Name:        tool.GetName(),
			Description: tool.Description,
			InputSchema: jsonSchema,
			Annotations: annotationsJSON,
		}
		if err := m.db.Create(t).Error; err != nil {
			// 单个工具写库失败只记录日志，尽量保留同一 Server 的其他可用工具。
			log.Printf("[ERROR] failed to register tool %s in DB: %v", canonicalToolName, err)
			continue
		}

		// 数据库保存原名，加入代理前改成 server__tool 规范名。
		tool.Name = canonicalToolName

		if s.Transport == types.TransportSSE {
			m.sseMcpProxyServer.AddTool(tool, m.MCPProxyToolCallHandler)
		} else {
			m.mcpProxyServer.AddTool(tool, m.MCPProxyToolCallHandler)
		}

		// 三份状态按“数据库 -> 代理 -> 内存索引”顺序建立。
		m.addToolInstance(tool)
		// 最后通知 Tool Group 等派生模块。
		m.notifyToolAddition(tool.Name)
	}
	return nil
}

// deregisterServerTools 从数据库、代理目录和内存索引中移除某个 Server 的全部工具。
func (m *MCPService) deregisterServerTools(s *model.McpServer) error {
	// 删除数据库记录前先取得规范名列表，后续代理删除需要使用这些名称。
	tools, err := m.ListToolsByServer(s.Name)
	if err != nil {
		return fmt.Errorf("failed to list tools for server %s: %w", s.Name, err)
	}

	// 使用 Unscoped 物理删除，避免注销后留下软删除记录影响同名重新注册。
	result := m.db.Unscoped().Where("server_id = ?", s.ID).Delete(&model.Tool{})
	if result.Error != nil {
		return fmt.Errorf("failed to delete tools for server %s: %w", s.Name, result.Error)
	}

	// 按 Server Transport 从正确的代理实例删除。
	toolNames := make([]string, len(tools))
	for i, tool := range tools {
		toolNames[i] = tool.Name
	}

	if s.Transport == types.TransportSSE {
		m.sseMcpProxyServer.DeleteTools(toolNames...)
	} else {
		m.mcpProxyServer.DeleteTools(toolNames...)
	}

	// 清理内存索引。
	m.deleteToolInstances(toolNames...)

	// 通知所有派生视图同步删除。
	m.notifyToolDeletion(toolNames...)

	return nil
}

// addToolInstance 写入内存索引；同名项直接覆盖，适合重新启用时刷新定义。
func (m *MCPService) addToolInstance(tool mcp.Tool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolInstances[tool.GetName()] = tool
}

// deleteToolInstances 批量删除内存 Tool，并用写锁保护并发读写。
func (m *MCPService) deleteToolInstances(toolNames ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range toolNames {
		delete(m.toolInstances, name)
	}
}

// notifyToolDeletion 将删除事件交给已注册的依赖方。
func (m *MCPService) notifyToolDeletion(toolNames ...string) {
	m.toolDeletionCallback(toolNames...)
}

// notifyToolAddition 采用尽力而为策略：主工具已成功注册时，派生视图更新失败只记日志，不回滚主流程。
func (m *MCPService) notifyToolAddition(toolName string) {
	if err := m.toolAdditionCallback(toolName); err != nil {
		// 此时数据库和主代理已经成功，不因附属回调失败制造更大的状态回滚问题。
		log.Printf("[ERROR] tool addition callback failed for tool %s: %v", toolName, err)
	}
}

// convertToolCallResToAPIRes 把 mcp-go SDK 响应转换成项目自己的 REST DTO，隔离外部 SDK 类型。
func (m *MCPService) convertToolCallResToAPIRes(resp *mcp.CallToolResult) (*types.ToolInvokeResult, error) {
	// Content 是多态接口，需要先转换为通用 JSON 对象。
	contentList, err := m.convertToolCallRespContent(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("failed to convert content: %w", err)
	}

	// Meta 中既可能有标准字段，也可能有协议扩展字段。
	metaMap := m.convertMCPMetaToMap(resp.Meta)

	return &types.ToolInvokeResult{
		Meta:              metaMap,
		IsError:           resp.IsError,
		Content:           contentList,
		StructuredContent: resp.StructuredContent,
	}, nil
}

// convertToolCallRespContent 借助 JSON 编解码保留不同 Content 实现的字段，而无需逐种类型判断。
func (m *MCPService) convertToolCallRespContent(content []mcp.Content) ([]map[string]any, error) {
	if len(content) == 0 {
		return []map[string]any{}, nil
	}

	contentList := make([]map[string]any, 0, len(content))

	for i, item := range content {
		// 每项独立转换，错误信息保留下标，方便定位异常上游响应。
		serialized, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal content item %d: %w", i, err)
		}

		var contentMap map[string]any
		if err := json.Unmarshal(serialized, &contentMap); err != nil {
			return nil, fmt.Errorf("failed to unmarshal content item %d: %w", i, err)
		}

		contentList = append(contentList, contentMap)
	}

	return contentList, nil
}

// convertMCPMetaToMap 合并协议扩展字段和标准 progressToken，并保持 nil 语义。
func (m *MCPService) convertMCPMetaToMap(meta *mcp.Meta) map[string]any {
	if meta == nil {
		return nil
	}

	// 先复制扩展字段，避免丢失未来新增的协议信息。
	metaMap := make(map[string]any)
	if meta.AdditionalFields != nil {
		// 浅拷贝即可，这里只负责构造响应 DTO。
		for k, v := range meta.AdditionalFields {
			metaMap[k] = v
		}
	}

	// progressToken 是 SDK 单独建模的标准字段，需要显式补回 map。
	if meta.ProgressToken != nil {
		metaMap["progressToken"] = meta.ProgressToken
	}

	// 空 Meta 返回 nil，而不是空对象，保持原 MCP 响应语义。
	if len(metaMap) == 0 {
		return nil
	}

	return metaMap
}
