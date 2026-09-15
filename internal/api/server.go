// Package api 是网关的传输层：用 Gin 暴露管理 REST API、Dashboard，以及供 AI 客户端连接的 MCP 端点。
package api

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zcray0326/mcp-gateway/internal/dashboardui"
	"github.com/zcray0326/mcp-gateway/internal/model"
	"github.com/zcray0326/mcp-gateway/internal/service/config"
	"github.com/zcray0326/mcp-gateway/internal/service/dashboard"
	"github.com/zcray0326/mcp-gateway/internal/service/mcp"
	"github.com/zcray0326/mcp-gateway/internal/service/mcpclient"
	"github.com/zcray0326/mcp-gateway/internal/service/toolgroup"
	"github.com/zcray0326/mcp-gateway/internal/service/user"
	"github.com/zcray0326/mcp-gateway/internal/telemetry"
	"github.com/zcray0326/mcp-gateway/pkg/types"
	"github.com/zcray0326/mcp-gateway/pkg/version"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

const (
	V0PathPrefix    = "/v0"
	V0ApiPathPrefix = "/api" + V0PathPrefix
)

type ServerOptions struct {
	// MCPProxyServer 聚合 STDIO 和 Streamable HTTP 上游，对外统一通过 /mcp 提供服务。
	MCPProxyServer *server.MCPServer
	// SseMcpProxyServer 单独承载旧 SSE Transport。
	// SSE 的握手和消息端点与 Streamable HTTP 不同，分开实例可避免两套协议状态互相干扰。
	SseMcpProxyServer *server.MCPServer

	MCPService       *mcp.MCPService
	MCPClientService *mcpclient.McpClientService
	ConfigService    *config.ServerConfigService
	UserService      *user.UserService
	ToolGroupService *toolgroup.ToolGroupService
	DashboardService *dashboard.Service

	OtelProviders *telemetry.Providers
	Metrics       telemetry.CustomMetrics
}

// Server 汇总 HTTP 层所需的各领域服务，并持有最终的 Gin Router。
// 它本身主要负责路由编排，具体注册、调用和持久化逻辑下沉到 service 层。
type Server struct {
	router *gin.Engine

	mcpProxyServer    *server.MCPServer
	sseMcpProxyServer *server.MCPServer

	mcpService       *mcp.MCPService
	mcpClientService *mcpclient.McpClientService

	configService    *config.ServerConfigService
	userService      *user.UserService
	toolGroupService *toolgroup.ToolGroupService
	dashboardService *dashboard.Service

	otelProviders *telemetry.Providers
	metrics       telemetry.CustomMetrics

	// 每个 Tool Group 的 SSE 连接都需要独立 server 实例；sync.Map 保护运行期间的并发创建和读取。
	groupSseServers sync.Map

	// Dashboard OAuth 结果只是供浏览器轮询的短期内存状态，互斥锁保护回调与轮询并发访问。
	dashboardOAuthMu      sync.Mutex
	dashboardOAuthResults map[string]dashboardOAuthSessionResult
}

// dashboardOAuthSessionResult 是展示层状态，不是 OAuth 会话的持久化真相来源。
// 它只负责把回调完成、失败或过期结果交给前端轮询接口。
type dashboardOAuthSessionResult struct {
	Status     string
	Error      string
	ServerName string
	ExpiresAt  time.Time
	UpdatedAt  time.Time
}

// NewServer 完成依赖绑定并创建路由。此处不启动端口监听，便于外层配置优雅关闭。
func NewServer(opts *ServerOptions) (*Server, error) {
	s := &Server{
		mcpProxyServer:        opts.MCPProxyServer,
		sseMcpProxyServer:     opts.SseMcpProxyServer,
		mcpService:            opts.MCPService,
		mcpClientService:      opts.MCPClientService,
		configService:         opts.ConfigService,
		userService:           opts.UserService,
		toolGroupService:      opts.ToolGroupService,
		dashboardService:      opts.DashboardService,
		otelProviders:         opts.OtelProviders,
		metrics:               opts.Metrics,
		dashboardOAuthResults: make(map[string]dashboardOAuthSessionResult),
	}

	// 等依赖字段全部就绪后再注册 Handler，避免闭包捕获到未初始化服务。
	r, err := s.setupRouter()
	if err != nil {
		return nil, err
	}
	s.router = r

	return s, nil
}

// IsInitialized 从持久化配置判断网关是否已完成首次初始化。
func (s *Server) IsInitialized() (bool, error) {
	c, err := s.configService.GetConfig()
	if err != nil {
		return false, fmt.Errorf("failed to get server config: %w", err)
	}
	return c.Initialized, nil
}

// GetMode 返回当前 Dev/Enterprise 模式；未初始化时不允许猜测默认值。
func (s *Server) GetMode() (model.ServerMode, error) {
	ok, err := s.IsInitialized()
	if err != nil {
		return "", fmt.Errorf("failed to check if server is initialized: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("server is not initialized")
	}
	c, err := s.configService.GetConfig()
	if err != nil {
		return "", fmt.Errorf("failed to get server config: %w", err)
	}
	return c.Mode, nil
}

// InitDev 为本地模式写入初始化配置。Dev 模式没有多用户体系，因此不会创建管理员。
func (s *Server) InitDev() error {
	_, err := s.configService.Init(model.ModeDev)
	if err != nil {
		return fmt.Errorf("failed to initialize server config in dev mode: %w", err)
	}
	return nil
}

// Router 暴露标准 http.Handler，由 cmd/start.go 自己创建 http.Server，以支持优雅关闭。
func (s *Server) Router() http.Handler {
	return s.router
}

// setupRouter 集中声明三类入口：运维端点、MCP 代理端点和 REST 管理端点。
// 阅读时重点关注每个 Group 挂载的中间件，它决定了初始化、身份认证和角色授权顺序。
func (s *Server) setupRouter() (*gin.Engine, error) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// 开启可观测性后同时采集 HTTP 指标，并暴露 Prometheus 拉取端点。
	if s.otelProviders != nil && s.otelProviders.IsEnabled() {
		// otelgin 自动记录请求耗时、状态码等 HTTP 指标。
		r.Use(otelgin.Middleware(s.otelProviders.ServiceName()))

		// Prometheus 通过 /metrics 主动抓取进程内指标。
		r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	}

	r.GET(
		"/health",
		func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		},
	)

	r.GET(
		"/metadata",
		func(c *gin.Context) {
			m := &types.ServerMetadata{
				Version: version.GetVersion(),
			}
			c.JSON(http.StatusOK, m)
		},
	)

	r.POST("/init", s.registerInitServerHandler())

	requireEnterpriseMode := s.requireServerMode(model.ModeEnterprise)
	requireDashboardMode := s.requireDashboardMode()

	if s.dashboardService != nil {
		dashboardFileServer, err := dashboardui.FileServer()
		if err != nil {
			return nil, err
		}
		r.GET("/", s.requireInitialized(), requireDashboardMode, gin.WrapH(dashboardFileServer))
		r.GET("/index.html", s.requireInitialized(), requireDashboardMode, gin.WrapH(dashboardFileServer))
		r.GET("/assets/*filepath", s.requireInitialized(), requireDashboardMode, gin.WrapH(dashboardFileServer))
	}

	// /mcp 是推荐的 Streamable HTTP 统一入口，客户端只需配置这一个地址。
	streamableHTTPServer := server.NewStreamableHTTPServer(s.mcpProxyServer)
	r.Any(
		"/mcp",
		s.requireInitialized(),
		s.checkAuthForMcpProxyAccess(),
		gin.WrapH(streamableHTTPServer),
	)

	r.Any(
		V0PathPrefix+"/groups/:name/mcp",
		s.requireInitialized(),
		s.checkAuthForMcpProxyAccess(),
		s.toolGroupMCPServerCallHandler(),
	)

	// /sse 与 /message 成对提供旧版 SSE Transport，仅用于兼容历史客户端。
	sseServer := server.NewSSEServer(s.sseMcpProxyServer)
	r.Any(
		"/sse",
		s.requireInitialized(),
		s.checkAuthForMcpProxyAccess(),
		gin.WrapH(sseServer.SSEHandler()),
	)
	r.Any(
		"/message",
		s.requireInitialized(),
		s.checkAuthForMcpProxyAccess(),
		gin.WrapH(sseServer.MessageHandler()),
	)

	r.Any(
		V0PathPrefix+"/groups/:name/sse",
		s.requireInitialized(),
		s.checkAuthForMcpProxyAccess(),
		s.toolGroupSseMCPServerCallHandler(),
	)
	r.Any(
		V0PathPrefix+"/groups/:name/message",
		s.requireInitialized(),
		s.checkAuthForMcpProxyAccess(),
		s.toolGroupSseMCPServerCallMessageHandler(),
	)

	// /api/v0 是 CLI 和外部管理程序使用的 REST API；先检查初始化，再验证管理用户身份。
	apiV0 := r.Group(
		V0ApiPathPrefix,
		s.requireInitialized(),
		s.verifyUserAuthForAPIAccess(),
	)

	// 普通用户可执行发现和调用；Dev 模式下相当于本机匿名用户。
	userAPI := apiV0.Group("/")
	{
		userAPI.GET("/servers", s.listServersHandler())

		userAPI.GET("/tools", s.listToolsHandler())
		userAPI.POST("/tools/invoke", s.invokeToolHandler())
		userAPI.GET("/tool", s.getToolHandler())

		userAPI.GET("/resources", s.listResourcesHandler())
		userAPI.POST("/resources/get", s.getResourceHandler())
		userAPI.POST("/resources/read", s.readResourceHandler())

		// Prompt 的查询和带参数渲染接口。
		userAPI.GET("/prompts", s.listPromptsHandler())
		userAPI.GET("/prompt", s.getPromptHandler())
		userAPI.POST("/prompts/render", s.getPromptWithArgsHandler())

		userAPI.GET("/users/whoami", requireEnterpriseMode, s.whoAmIHandler())
	}

	// 修改注册表、凭据、用户和客户端属于管理操作，Enterprise 模式要求管理员角色。
	adminAPI := apiV0.Group("/", s.requireAdminUser())
	{
		adminAPI.POST("/servers", s.registerServerHandler())
		adminAPI.POST("/upstream_oauth/sessions/:id/complete", s.completeUpstreamOAuthSessionHandler())
		adminAPI.DELETE("/servers/:name", s.deregisterServerHandler())
		adminAPI.POST("/servers/:name/enable", s.enableServerHandler())
		adminAPI.POST("/servers/:name/disable", s.disableServerHandler())

		// 完整 Server 配置可能包含 Bearer Token 等凭据，只允许管理员导出。
		adminAPI.GET("/server_configs", s.getServerConfigsHandler())

		adminAPI.POST("/tools/enable", s.enableToolsHandler())
		adminAPI.POST("/tools/disable", s.disableToolsHandler())

		adminAPI.POST("/prompts/enable", s.enablePromptsHandler())
		adminAPI.POST("/prompts/disable", s.disablePromptsHandler())

		// MCP Client 代表 Claude、Cursor 等调用方，与人类管理用户不是同一实体。
		adminAPI.GET(
			"/clients",
			requireEnterpriseMode,
			s.listMcpClientsHandler(),
		)
		adminAPI.POST(
			"/clients",
			requireEnterpriseMode,
			s.createMcpClientHandler(),
		)
		adminAPI.PUT(
			"/clients/:name",
			requireEnterpriseMode,
			s.updateMcpClientHandler(),
		)
		adminAPI.DELETE(
			"/clients/:name",
			requireEnterpriseMode,
			s.deleteMcpClientHandler(),
		)

		// 人类用户用于登录管理 API，仅 Enterprise 模式启用。
		adminAPI.POST(
			"/users",
			requireEnterpriseMode,
			s.createUserHandler(),
		)
		adminAPI.GET(
			"/users",
			requireEnterpriseMode,
			s.listUsersHandler(),
		)
		adminAPI.DELETE(
			"/users/:username",
			requireEnterpriseMode,
			s.deleteUserHandler(),
		)
		adminAPI.PUT(
			"/users/:username",
			requireEnterpriseMode,
			s.updateUserHandler(),
		)

		// Tool Group 将完整工具集合裁剪成面向特定客户端的子集。
		adminAPI.POST("/tool-groups", s.createToolGroupHandler())
		adminAPI.GET("/tool-groups/:name", s.getToolGroupHandler())
		adminAPI.GET("/tool-groups/:name/effective-tools", s.getToolGroupEffectiveToolsHandler())
		adminAPI.GET("/tool-groups", s.listToolGroupsHandler())
		adminAPI.DELETE("/tool-groups/:name", s.deleteToolGroupHandler())
		adminAPI.PUT("/tool-groups/:name", s.updateToolGroupHandler())
	}

	if s.dashboardService != nil {
		dashboardAPI := r.Group(
			"/api/dashboard",
			s.requireInitialized(),
			requireDashboardMode,
		)
		{
			dashboardAPI.GET("/overview", s.dashboardOverviewHandler())
			dashboardAPI.GET("/servers", s.dashboardServersHandler())
			dashboardAPI.POST("/servers", s.dashboardRegisterServerHandler())
			dashboardAPI.GET("/oauth/callback", s.dashboardOAuthCallbackHandler())
			dashboardAPI.GET("/oauth/session/:id", s.dashboardOAuthSessionHandler())
			dashboardAPI.DELETE("/servers/:name", s.dashboardDeleteServerHandler())
			dashboardAPI.PATCH("/servers/:name/enabled", s.dashboardSetServerEnabledHandler())
			dashboardAPI.GET("/tools", s.dashboardToolsHandler())
			dashboardAPI.PATCH("/tools/:name/enabled", s.dashboardSetToolEnabledHandler())
			dashboardAPI.GET("/tool-groups", s.dashboardToolGroupsHandler())
			dashboardAPI.POST("/tool-groups", s.dashboardCreateToolGroupHandler())
			dashboardAPI.GET("/tool-groups/:name", s.dashboardGetToolGroupHandler())
			dashboardAPI.DELETE("/tool-groups/:name", s.dashboardDeleteToolGroupHandler())
			dashboardAPI.GET("/prompts", s.dashboardPromptsHandler())
			dashboardAPI.PATCH("/prompts/:name/enabled", s.dashboardSetPromptEnabledHandler())
			dashboardAPI.GET("/resources", s.dashboardResourcesHandler())
			dashboardAPI.GET("/diagnostics", s.dashboardDiagnosticsHandler())
		}
	}

	return r, nil
}
