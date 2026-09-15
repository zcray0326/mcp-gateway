package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/zcray0326/mcp-gateway/internal/model"
	"github.com/zcray0326/mcp-gateway/internal/service/mcp"
	"github.com/zcray0326/mcp-gateway/pkg/apierrors"
	"github.com/zcray0326/mcp-gateway/pkg/types"
)

func (s *Server) registerServerHandler() gin.HandlerFunc {
	// Handler 只处理 HTTP 输入输出；Transport 配置校验、OAuth 探测和能力发现由 MCPService 完成。
	return func(c *gin.Context) {
		force, err := parseForceQueryParam(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		var input types.RegisterServerInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		server, err := createServerModelFromInput(&input)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if force {
			// force 采用“先完整注销、再重新注册”，确保旧工具和会话不会残留。
			if _, err := s.mcpService.GetMcpServer(input.Name); err == nil {
				log.Printf("[INFO] force=true: deregistering existing MCP server %s before re-registration", input.Name)
				if err := s.mcpService.DeregisterMcpServer(input.Name); err != nil {
					c.JSON(
						http.StatusInternalServerError,
						gin.H{"error": fmt.Sprintf("Error deregistering existing server with name %s: %v", input.Name, err)},
					)
					return
				}
			} else if !errors.Is(err, apierrors.ErrNotFound) {
				c.JSON(
					http.StatusInternalServerError,
					gin.H{"error": fmt.Sprintf("Error checking for existing server with name %s: %v", input.Name, err)},
				)
				return
			}
		}

		initiatedBy := ""
		if authenticatedUser, exists := c.Get("user"); exists {
			if u, ok := authenticatedUser.(*model.User); ok {
				initiatedBy = u.Username
			}
		}

		if err := s.mcpService.RegisterMcpServerWithOAuthSupport(c, &input, server, force, initiatedBy); err != nil {
			var oauthErr *mcp.UpstreamOAuthAuthorizationPendingError
			if errors.As(err, &oauthErr) {
				// OAuth 挑战不是注册失败：返回授权地址和会话 ID，让客户端完成浏览器授权后继续注册。
				c.JSON(http.StatusAccepted, types.RegisterServerResult{
					AuthorizationRequired: &types.UpstreamOAuthAuthorizationRequired{
						SessionID:        oauthErr.SessionID,
						AuthorizationURL: oauthErr.AuthorizationURL,
						ExpiresAt:        oauthErr.ExpiresAt,
					},
				})
				return
			}

			handleServiceError(c, err)
			return
		}

		c.JSON(http.StatusCreated, types.RegisterServerResult{Server: &types.McpServer{
			Name:        server.Name,
			Transport:   string(server.Transport),
			Enabled:     server.Enabled,
			Description: server.Description,
			SessionMode: string(server.SessionMode),
			URL:         input.URL,
			Command:     input.Command,
			Args:        input.Args,
			Env:         input.Env,
		}})
	}
}

func parseForceQueryParam(c *gin.Context) (bool, error) {
	// 未传 force 与显式 false 等价；非法布尔值直接作为输入错误返回。
	if c.Query("force") == "" {
		return false, nil
	}

	force, err := strconv.ParseBool(c.Query("force"))
	if err != nil {
		return false, fmt.Errorf("invalid force query parameter: %w", err)
	}

	return force, nil
}

func (s *Server) completeUpstreamOAuthSessionHandler() gin.HandlerFunc {
	// 客户端完成上游授权后调用此接口，服务会交换 Token 并继续之前暂停的注册流程。
	return func(c *gin.Context) {
		sessionID := c.Param("id")

		var input types.CompleteUpstreamOAuthSessionInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		server, err := s.mcpService.CompleteUpstreamOAuthSession(c, sessionID, input.Code, input.State)
		if err != nil {
			handleServiceError(c, err)
			return
		}

		resp := &types.McpServer{
			Name:        server.Name,
			Transport:   string(server.Transport),
			Enabled:     server.Enabled,
			Description: server.Description,
			SessionMode: string(server.SessionMode),
		}
		switch server.Transport {
		case types.TransportStreamableHTTP:
			conf, confErr := server.GetStreamableHTTPConfig()
			if confErr == nil {
				resp.URL = conf.URL
			}
		case types.TransportStdio:
			conf, confErr := server.GetStdioConfig()
			if confErr == nil {
				resp.Command = conf.Command
				resp.Args = conf.Args
				resp.Env = conf.Env
			}
		case types.TransportSSE:
			conf, confErr := server.GetSSEConfig()
			if confErr == nil {
				resp.URL = conf.URL
			}
		}

		c.JSON(http.StatusCreated, types.RegisterServerResult{Server: resp})
	}
}

func (s *Server) deregisterServerHandler() gin.HandlerFunc {
	// 注销动作由 MCPService 级联删除能力、认证状态和缓存会话。
	return func(c *gin.Context) {
		name := c.Param("name")

		if err := s.mcpService.DeregisterMcpServer(name); err != nil {
			handleServiceError(c, err)
			return
		}

		c.Status(http.StatusNoContent)
	}
}

func (s *Server) listServersHandler() gin.HandlerFunc {
	// 列表接口返回适合展示的摘要，不暴露完整连接凭据。
	return func(c *gin.Context) {
		records, err := s.mcpService.ListMcpServers()
		if err != nil {
			handleServiceError(c, err)
			return
		}

		servers := make([]*types.McpServer, len(records))

		for i, record := range records {
			servers[i] = &types.McpServer{
				Name:        record.Name,
				Transport:   string(record.Transport),
				Enabled:     record.Enabled,
				Description: record.Description,
				SessionMode: string(record.SessionMode),
			}

			switch record.Transport {
			case types.TransportStreamableHTTP:
				conf, err := record.GetStreamableHTTPConfig()
				if err != nil {
					c.JSON(
						http.StatusInternalServerError,
						gin.H{
							"error": fmt.Sprintf("Error getting streamable HTTP config for server %s: %v", record.Name, err),
						},
					)
					return
				}
				servers[i].URL = conf.URL
			case types.TransportStdio:
				conf, err := record.GetStdioConfig()
				if err != nil {
					c.JSON(
						http.StatusInternalServerError,
						gin.H{
							"error": fmt.Sprintf("Error getting stdio config for server %s: %v", record.Name, err),
						},
					)
					return
				}
				servers[i].Command = conf.Command
				servers[i].Args = conf.Args
				servers[i].Env = conf.Env
			default:
				// 剩余网络分支为兼容用 SSE。
				conf, err := record.GetSSEConfig()
				if err != nil {
					c.JSON(
						http.StatusInternalServerError,
						gin.H{
							"error": fmt.Sprintf("Error getting SSE config for server %s: %v", record.Name, err),
						},
					)
					return
				}
				servers[i].URL = conf.URL
			}
		}

		c.JSON(http.StatusOK, servers)
	}
}

func (s *Server) enableServerHandler() gin.HandlerFunc {
	// Server 启用会级联恢复其全部能力到运行时代理。
	return func(c *gin.Context) {
		name := c.Param("name")

		tools, prompts, err := s.mcpService.EnableMcpServer(name)
		if err != nil {
			handleServiceError(c, err)
			return
		}

		result := types.EnableDisableServerResult{
			Name:            name,
			ToolsAffected:   tools,
			PromptsAffected: prompts,
		}
		c.JSON(http.StatusOK, result)
	}
}

func (s *Server) disableServerHandler() gin.HandlerFunc {
	// Server 停用会级联从代理目录移除能力，但保留注册配置。
	return func(c *gin.Context) {
		name := c.Param("name")

		tools, prompts, err := s.mcpService.DisableMcpServer(name)
		if err != nil {
			handleServiceError(c, err)
			return
		}

		result := types.EnableDisableServerResult{
			Name:            name,
			ToolsAffected:   tools,
			PromptsAffected: prompts,
		}
		c.JSON(http.StatusOK, result)
	}
}

// getServerConfigsHandler 导出可用于重新注册的完整配置，其中可能包含 Header 或 Bearer Token。
// 因此它与普通列表接口分开，并在路由层限制为管理员访问。
func (s *Server) getServerConfigsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		records, err := s.mcpService.ListMcpServers()
		if err != nil {
			handleServiceError(c, err)
			return
		}

		servers := make([]*types.RegisterServerInput, len(records))

		for i, record := range records {
			servers[i] = &types.RegisterServerInput{
				Name:        record.Name,
				Transport:   string(record.Transport),
				Description: record.Description,
				SessionMode: string(record.SessionMode),
			}

			switch record.Transport {
			case types.TransportStreamableHTTP:
				conf, err := record.GetStreamableHTTPConfig()
				if err != nil {
					c.JSON(
						http.StatusInternalServerError,
						gin.H{
							"error": fmt.Sprintf("Error getting streamable HTTP config for server %s: %v", record.Name, err),
						},
					)
					return
				}
				servers[i].URL = conf.URL
				servers[i].BearerToken = conf.BearerToken
				servers[i].Headers = conf.Headers
			case types.TransportStdio:
				conf, err := record.GetStdioConfig()
				if err != nil {
					c.JSON(
						http.StatusInternalServerError,
						gin.H{
							"error": fmt.Sprintf("Error getting stdio config for server %s: %v", record.Name, err),
						},
					)
					return
				}
				servers[i].Command = conf.Command
				servers[i].Args = conf.Args
				servers[i].Env = conf.Env
			default:
				// 剩余网络分支为 SSE 配置。
				conf, err := record.GetSSEConfig()
				if err != nil {
					c.JSON(
						http.StatusInternalServerError,
						gin.H{
							"error": fmt.Sprintf("Error getting SSE config for server %s: %v", record.Name, err),
						},
					)
					return
				}
				servers[i].URL = conf.URL
				servers[i].BearerToken = conf.BearerToken
			}

			if oauthToken, err := s.mcpService.GetUpstreamOAuthToken(record.Name); err == nil {
				servers[i].OAuthRedirectURI = oauthToken.RedirectURI
				servers[i].OAuthClientID = oauthToken.ClientID
				servers[i].OAuthClientSecret = oauthToken.ClientSecret
				scopes, scopeErr := mcp.ScopesFromJSONForAPI(oauthToken.Scopes)
				if scopeErr == nil {
					servers[i].OAuthScopes = scopes
				}
			}
		}

		c.JSON(http.StatusOK, servers)
	}
}

func createServerModelFromInput(input *types.RegisterServerInput) (*model.McpServer, error) {
	// REST DTO 支持三种互斥 Transport，这里把它归一成数据库统一存储的 McpServer 模型。
	transport, err := types.ValidateTransport(input.Transport)
	if err != nil {
		return nil, err
	}

	sessionMode, err := types.ValidateSessionMode(input.SessionMode)
	if err != nil {
		return nil, err
	}

	switch transport {
	case types.TransportStreamableHTTP:
		server, err := model.NewStreamableHTTPServer(
			input.Name,
			input.Description,
			input.URL,
			input.BearerToken,
			input.Headers,
			sessionMode,
		)
		if err != nil {
			return nil, fmt.Errorf("error creating streamable http server: %v", err)
		}
		return server, nil
	case types.TransportStdio:
		server, err := model.NewStdioServer(
			input.Name,
			input.Description,
			input.Command,
			input.Args,
			input.Env,
			sessionMode,
		)
		if err != nil {
			return nil, fmt.Errorf("error creating stdio server: %v", err)
		}
		return server, nil
	default:
		server, err := model.NewSSEServer(
			input.Name,
			input.Description,
			input.URL,
			input.BearerToken,
			sessionMode,
		)
		if err != nil {
			return nil, fmt.Errorf("error creating SSE server: %v", err)
		}
		return server, nil
	}
}
