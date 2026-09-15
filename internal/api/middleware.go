package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zcray0326/mcp-gateway/internal/model"
	"github.com/zcray0326/mcp-gateway/pkg/types"
)

// requireInitialized 是受保护路由的第一道门：服务未初始化时拒绝访问，并把运行模式写入 Gin 上下文。
// 后续鉴权中间件依赖这个 mode，因此路由注册时必须保证调用顺序。
func (s *Server) requireInitialized() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg, err := s.configService.GetConfig()
		if err != nil || !cfg.Initialized {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "server is not initialized"})
			return
		}
		// 同一次 HTTP 请求内复用 mode，避免每层中间件重复查询数据库。
		c.Set("mode", cfg.Mode)
		c.Next()
	}
}

// requireDashboardMode 只允许 Dev 模式访问 Dashboard。
// Enterprise 模式返回 404 而不是 403，避免向外暴露未开放的管理界面路径。
func (s *Server) requireDashboardMode() gin.HandlerFunc {
	return func(c *gin.Context) {
		mode, exists := c.Get("mode")
		if !exists {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "server mode not found in context"})
			return
		}
		currentMode, ok := mode.(model.ServerMode)
		if !ok {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "invalid server mode in context"})
			return
		}
		if currentMode != model.ModeDev {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// verifyUserAuthForAPIAccess 验证“人类管理用户”的 Access Token。
// 它只确认身份，不判断管理员角色；角色授权由 requireAdminUser 继续处理。
func (s *Server) verifyUserAuthForAPIAccess() gin.HandlerFunc {
	return func(c *gin.Context) {
		mode, exists := c.Get("mode")
		if !exists {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "server mode not found in context"})
			return
		}
		m, ok := mode.(model.ServerMode)
		if !ok {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "invalid server mode in context"})
			return
		}
		if m == model.ModeDev {
			// Dev 模式定位为本地单人使用，因此跳过用户鉴权。
			c.Next()
			return
		}

		authHeader := c.GetHeader("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing access token"})
			return
		}

		// Token 必须能查到仍有效的用户记录。
		authenticatedUser, err := s.userService.GetUserByAccessToken(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid access token: " + err.Error()})
			return
		}

		// 将完整用户对象交给后续角色中间件，避免再次查库。
		c.Set("user", authenticatedUser)
		c.Next()
	}
}

// requireAdminUser 实现管理接口的角色授权。
// 它假设身份认证已经完成，Enterprise 模式下仅管理员可以继续请求。
func (s *Server) requireAdminUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		mode, exists := c.Get("mode")
		if !exists {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "server mode not found in context"})
			return
		}
		m, ok := mode.(model.ServerMode)
		if !ok {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "invalid server mode in context"})
			return
		}
		if m == model.ModeDev {
			// Dev 模式没有多人角色体系，直接放行。
			c.Next()
			return
		}

		authenticatedUser, exists := c.Get("user")
		if !exists {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "user is not authenticated"})
			return
		}

		u, ok := authenticatedUser.(*model.User)
		if ok && u.Role == types.UserRoleAdmin {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "user is not authorized to perform this action"})
	}
}

// requireServerMode 限制某些能力只能在指定模式中使用，不匹配时返回 403。
// 历史名称 ModeProd 与 ModeEnterprise 语义等价，因此二者互相兼容。
func (s *Server) requireServerMode(m model.ServerMode) gin.HandlerFunc {
	return func(c *gin.Context) {
		mode, exists := c.Get("mode")
		if !exists {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "server mode not found in context"})
			return
		}
		currentMode, ok := mode.(model.ServerMode)
		if !ok {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "invalid server mode in context"})
			return
		}

		if currentMode == m {
			// 模式完全一致，正常放行。
			c.Next()
			return
		}
		if model.IsEnterpriseMode(currentMode) && model.IsEnterpriseMode(m) {
			// production 是 enterprise 的旧名称，两者视为同一模式。
			c.Next()
			return
		}
		// 当前模式不具备该路由所需能力。
		c.AbortWithStatusJSON(
			http.StatusForbidden,
			gin.H{"error": fmt.Sprintf("this request is only allowed in %s mode", m)},
		)
	}
}

// checkAuthForMcpProxyAccess 验证“AI/MCP 客户端”的 Token，与管理用户鉴权是两套身份体系。
// 认证成功后把客户端对象写入标准 request.Context，供下层 MCP 代理执行 Server 级访问控制。
func (s *Server) checkAuthForMcpProxyAccess() gin.HandlerFunc {
	return func(c *gin.Context) {
		mode, exists := c.Get("mode")
		if !exists {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "server mode not found in context"})
			return
		}
		m, ok := mode.(model.ServerMode)
		if !ok {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "invalid server mode in context"})
			return
		}

		// mcp-go 看不到 Gin Context，所以必须把 mode 转存到标准库 request.Context。
		ctx := context.WithValue(c.Request.Context(), "mode", m)
		c.Request = c.Request.WithContext(ctx)

		if m == model.ModeDev {
			// Dev 模式不要求 MCP Client Token。
			c.Next()
			return
		}

		authHeader := c.GetHeader("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing MCP client access token"})
			return
		}
		client, err := s.mcpClientService.GetClientByToken(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid MCP client token"})
			return
		}

		// 代理层会读取 client，并校验它是否被允许访问目标上游 Server。
		ctx = context.WithValue(c.Request.Context(), "client", client)
		c.Request = c.Request.WithContext(ctx)

		c.Next()
	}
}
