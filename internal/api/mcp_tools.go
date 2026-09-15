package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zcray0326/mcp-gateway/internal/model"
)

// listToolsHandler 负责工具发现：带 server 查询参数时按上游筛选，否则返回全部规范名工具。
func (s *Server) listToolsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		server := c.Query("server")
		var (
			tools []model.Tool
			err   error
		)
		if server == "" {
			// 未指定 Server，返回整个网关聚合后的工具目录。
			tools, err = s.mcpService.ListTools()
		} else {
			// 指定 Server，只查询该上游的工具。
			tools, err = s.mcpService.ListToolsByServer(server)
		}
		if err != nil {
			handleServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, tools)
	}
}

// invokeToolHandler 是 REST 形式的工具调用入口，解析名称和参数后交给 MCPService 执行。
func (s *Server) invokeToolHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var args map[string]any
		if err := json.NewDecoder(c.Request.Body).Decode(&args); err != nil {
			c.JSON(
				http.StatusBadRequest,
				gin.H{"error": "failed to decode request body: " + err.Error()},
			)
			return
		}

		rawName, ok := args["name"]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing 'name' field in request body"})
			return
		}
		name, ok := rawName.(string)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "'name' field must be a string"})
			return
		}

		// name 是网关路由参数，不属于工具自己的 arguments，调用前必须删除。
		delete(args, "name")

		resp, err := s.mcpService.InvokeTool(c, name, args)
		if err != nil {
			handleServiceError(c, fmt.Errorf("failed to invoke tool: %w", err))
			return
		}

		c.JSON(http.StatusOK, resp)
	}
}

// getToolHandler 按 server__tool 规范名返回单个工具定义。
func (s *Server) getToolHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 工具名可能包含斜杠，使用查询参数可避免与 Gin 路径分段冲突。
		name := c.Query("name")
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing 'name' query parameter"})
			return
		}

		tool, err := s.mcpService.GetTool(name)
		if err != nil {
			handleServiceError(c, fmt.Errorf("failed to get tool: %w", err))
			return
		}

		c.JSON(http.StatusOK, tool)
	}
}

// enableToolsHandler 启用单个工具或某个 Server 的全部工具。
func (s *Server) enableToolsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		entity := c.Query("entity")
		if entity == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing 'entity' query parameter"})
			return
		}
		enabledTools, err := s.mcpService.EnableTools(entity)
		if err != nil {
			handleServiceError(c, fmt.Errorf("failed to enable tool(s): %w", err))
			return
		}
		c.JSON(http.StatusOK, enabledTools)
	}
}

// disableToolsHandler 停用单个工具或某个 Server 的全部工具。
func (s *Server) disableToolsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		entity := c.Query("entity")
		if entity == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing 'entity' query parameter"})
			return
		}
		disabledTools, err := s.mcpService.DisableTools(entity)
		if err != nil {
			handleServiceError(c, fmt.Errorf("failed to disable tool(s): %w", err))
			return
		}
		c.JSON(http.StatusOK, disabledTools)
	}
}
