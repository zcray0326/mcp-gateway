package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
	"github.com/zcray0326/mcp-gateway/internal/api"
	"github.com/zcray0326/mcp-gateway/internal/db"
	"github.com/zcray0326/mcp-gateway/internal/migrations"
	"github.com/zcray0326/mcp-gateway/internal/model"
	"github.com/zcray0326/mcp-gateway/internal/service/config"
	"github.com/zcray0326/mcp-gateway/internal/service/dashboard"
	"github.com/zcray0326/mcp-gateway/internal/service/mcp"
	"github.com/zcray0326/mcp-gateway/internal/service/mcpclient"
	"github.com/zcray0326/mcp-gateway/internal/service/toolgroup"
	"github.com/zcray0326/mcp-gateway/internal/service/user"
	"github.com/zcray0326/mcp-gateway/internal/telemetry"
	"github.com/zcray0326/mcp-gateway/pkg/version"
)

const (
	// TODO: 后续可增加 MCP_GATEWAY_BIND_PORT，同时保留 PORT 兼容旧配置。
	BindPortEnvVar  = "PORT"
	BindPortDefault = "8080"

	// BindHostEnvVar 控制监听网卡。空值表示所有网卡；127.0.0.1 表示仅本机可访问。
	// Dev 模式不做鉴权，因此本地使用时绑定回环地址更安全。
	BindHostEnvVar = "MCP_GATEWAY_BIND_HOST"

	DBUrlEnvVar            = "DATABASE_URL"
	SQLiteDBPathEnvVar     = "SQLITE_DB_PATH"
	ServerModeEnvVar       = "SERVER_MODE"
	TelemetryEnabledEnvVar = "OTEL_ENABLED"
)

const (
	PostgresHostEnvVar     = "POSTGRES_HOST"
	PostgresPortEnvVar     = "POSTGRES_PORT"
	PostgresUserEnvVar     = "POSTGRES_USER"
	PostgresPasswordEnvVar = "POSTGRES_PASSWORD"
	PostgresDBEnvVar       = "POSTGRES_DB"
)

const (
	// McpServerInitReqTimeoutSecEnvVar 控制与新上游完成 MCP initialize 握手的最长等待时间。
	McpServerInitReqTimeoutSecEnvVar = "MCP_SERVER_INIT_REQ_TIMEOUT_SEC"

	// McpServerInitRequestTimeoutSecondsDefault 是默认握手超时秒数。
	McpServerInitRequestTimeoutSecondsDefault = 30

	// SessionIdleTimeoutSecEnvVar 配置 Stateful 连接的空闲回收时间。
	SessionIdleTimeoutSecEnvVar = "SESSION_IDLE_TIMEOUT_SEC"

	// -1 交给 SessionManager 使用其默认值；0 表示完全不做空闲回收。
	SessionIdleTimeoutSecondsDefault = -1
)

var (
	startServerCmdBindHost          string
	startServerCmdBindPort          string
	startServerCmdSQLiteDBPath      string
	startServerCmdEnterpriseEnabled bool
	startServerCmdProdEnabled       bool
)

var startServerCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the MCP Gateway server",
	Long: "Starts the MCP Gateway HTTP Registry and the MCP Gateway\n\n" +
		"The server is started in development mode by default, which is ideal for running mcp-gateway locally.\n" +
		"Teams & Enterprises should run mcp-gateway in enterprise mode.\n\n" +
		"If no PostgreSQL configuration is provided, this command uses a SQLite database file at ./mcp-gateway.db by default.\n" +
		"You can optionally override that SQLite file path with the --sqlite-db-path flag or the SQLITE_DB_PATH environment variable.\n" +
		"You can also supply a custom DSN in the DATABASE_URL environment variable.\n" +
		"eg: export DATABASE_URL='postgres://user:password@localhost:5432/mcp-gateway'\n" +
		"For Postgres, you can also set individual connection details using the following environment variables:\n" +
		"POSTGRES_HOST, POSTGRES_PORT (default 5432), POSTGRES_USER (default postgres), POSTGRES_PASSWORD, POSTGRES_DB (default postgres)\n\n" +
		"You can also configure the amount of time (in seconds) mcp-gateway will wait for a new MCP server's initialization before aborting it.\n" +
		"Set the MCP_SERVER_INIT_REQ_TIMEOUT_SEC environment variable to an integer (default is 30).\n" +
		"This is useful when you register a MCP server (usually stdio, like filesystem) that may take some time to start up.\n\n" +
		"Finally, you can also configure the idle timeout (in seconds) for stateful sessions.\n" +
		"Set the SESSION_IDLE_TIMEOUT_SEC environment variable to an integer (default is -1, meaning no timeout).\n" +
		"This is useful to automatically clean up idle sessions after a certain period of inactivity.",
	RunE: runStartServer,
	Annotations: map[string]string{
		"group": string(subCommandGroupBasic),
		"order": "1",
	},
}

func init() {
	startServerCmd.Flags().StringVar(
		&startServerCmdBindPort,
		"port",
		"",
		fmt.Sprintf("port to bind the HTTP server to (overrides env var %s)", BindPortEnvVar),
	)
	startServerCmd.Flags().StringVar(
		&startServerCmdBindHost,
		"host",
		"",
		fmt.Sprintf(
			"host/interface to bind the HTTP server to, eg- 127.0.0.1 for local-only access "+
				"(overrides env var %s; empty means all interfaces)",
			BindHostEnvVar,
		),
	)
	startServerCmd.Flags().StringVar(
		&startServerCmdSQLiteDBPath,
		"sqlite-db-path",
		"",
		fmt.Sprintf(
			"path to a custom SQLite database file to use, if not using postgres; defaults to ./mcp-gateway.db (overrides env var %s)",
			SQLiteDBPathEnvVar,
		),
	)
	startServerCmd.Flags().BoolVar(
		&startServerCmdEnterpriseEnabled,
		"enterprise",
		false,
		fmt.Sprintf(
			"Run the server in Enterprise mode (ideal for teams and enterprises)."+
				" Alternatively, set the %s environment variable ('%s' | '%s')",
			ServerModeEnvVar, model.ModeDev, model.ModeEnterprise,
		),
	)
	startServerCmd.Flags().BoolVar(
		&startServerCmdProdEnabled,
		"prod",
		false,
		"[DEPRECATED] Alias for --enterprise flag.",
	)

	rootCmd.AddCommand(startServerCmd)
}

func newProxyServers() (*server.MCPServer, *server.MCPServer) {
	// 代理上报的 MCP Server 版本与网关二进制保持一致，避免写死后出现版本信息漂移。
	proxyVersion := version.GetVersion()

	mcpProxyServer := server.NewMCPServer(
		"MCP Gateway Proxy MCP Server",
		proxyVersion,
		server.WithResourceCapabilities(false, false),
		server.WithToolCapabilities(true),
		server.WithPromptCapabilities(true),
		server.WithToolFilter(mcp.ProxyToolFilter),
	)
	sseMcpProxyServer := server.NewMCPServer(
		"MCP Gateway Proxy MCP Server for SSE transport",
		proxyVersion,
		server.WithResourceCapabilities(false, false),
		server.WithToolCapabilities(true),
		server.WithPromptCapabilities(true),
		server.WithToolFilter(mcp.ProxyToolFilter),
	)

	return mcpProxyServer, sseMcpProxyServer
}

// getDesiredServerMode 按“命令行参数 > 环境变量 > Dev 默认值”的优先级决定运行模式。
func getDesiredServerMode(cmd *cobra.Command) (model.ServerMode, error) {
	desiredServerMode := model.ModeDev

	envMode := os.Getenv(ServerModeEnvVar)
	if envMode != "" {
		// 模式配置不区分大小写，降低环境变量配置出错概率。
		envMode = strings.ToLower(envMode)

		// production 是旧名称，内部统一归一为 enterprise。
		if envMode == string(model.ModeProd) {
			cmd.Printf(
				"Warning: '%s' value is deprecated for env var %s, please use '%s' instead\n\n",
				model.ModeProd, ServerModeEnvVar, model.ModeEnterprise,
			)
			envMode = string(model.ModeEnterprise)
		}

		if envMode != string(model.ModeDev) && envMode != string(model.ModeEnterprise) {
			return "", fmt.Errorf(
				"invalid value for %s environment variable: '%s', valid values are '%s' and '%s'",
				ServerModeEnvVar, envMode, model.ModeDev, model.ModeEnterprise,
			)
		}

		desiredServerMode = model.ServerMode(envMode)
	}

	// 显式命令行参数覆盖环境变量；--prod 仅作为兼容别名保留。
	if startServerCmdEnterpriseEnabled || startServerCmdProdEnabled {
		desiredServerMode = model.ModeEnterprise
	}
	if startServerCmdProdEnabled {
		cmd.Println("Warning: --prod flag is deprecated, please use --enterprise flag instead")
	}

	return desiredServerMode, nil
}

// isTelemetryEnabled 决定是否采集指标：环境变量优先，否则 Dev 默认关闭、Enterprise 默认开启。
func isTelemetryEnabled(desiredServerMode model.ServerMode) (bool, error) {
	telemetryEnabled := desiredServerMode == model.ModeEnterprise

	envTelemetryEnabled := os.Getenv(TelemetryEnabledEnvVar)
	if envTelemetryEnabled != "" {
		envTelemetryEnabled = strings.ToLower(envTelemetryEnabled)

		switch envTelemetryEnabled {
		case "true", "1":
			telemetryEnabled = true
		case "false", "0":
			telemetryEnabled = false
		default:
			return false, fmt.Errorf(
				"invalid value for %s environment variable: '%s', valid values are 'true' or 'false'",
				TelemetryEnabledEnvVar, envTelemetryEnabled,
			)
		}
	}

	return telemetryEnabled, nil
}

// getBindHost returns the interface to bind to.
// precedence: command line flag > environment variable > all interfaces
// An explicit empty --host is meaningful: it forces the historical all-interface
// bind even when MCP_GATEWAY_BIND_HOST is set.
func getBindHost(cmd *cobra.Command) string {
	if cmd.Flags().Changed("host") {
		return startServerCmdBindHost
	}
	return os.Getenv(BindHostEnvVar)
}

// getBindPort returns the TCP port to bind the mcp-gateway server to
// precedence: command line flag > environment variable > default
func getBindPort() string {
	port := startServerCmdBindPort
	if port == "" {
		port = os.Getenv(BindPortEnvVar)
	}
	if port == "" {
		port = BindPortDefault
	}
	return port
}

// getSQLiteDBPathOverride returns the configured SQLite DB path override.
// precedence: command line flag > environment variable > unset (empty string)
func getSQLiteDBPathOverride() string {
	if startServerCmdSQLiteDBPath != "" {
		return strings.TrimSpace(startServerCmdSQLiteDBPath)
	}
	return strings.TrimSpace(os.Getenv(SQLiteDBPathEnvVar))
}

// getEnvOrFile returns the value of the given environment variable.
// If the environment variable is not set, it checks for a corresponding
// _FILE environment variable and reads the value from the file if it exists.
// If neither is set, it returns an empty string.
// If both are set, the value of the original environment variable takes precedence.
func getEnvOrFile(envVar string) (string, error) {
	val := os.Getenv(envVar)
	if val != "" {
		return val, nil
	}

	fileEnvVar := envVar + "_FILE"
	filePath := os.Getenv(fileEnvVar)
	if filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("failed to read %s: %w", fileEnvVar, err)
		}
		return strings.TrimSpace(string(data)), nil
	}

	return "", nil
}

// getPostgresDSN constructs a Postgres DSN from individual Postgres-specific environment variables & files.
// It is used to provide an alternative way to specify Postgres connection details
// in case the user doesn't want to use a full DATABASE_URL.
// If POSTGRES_HOST is not set, this function assumes that Postgres-specific env vars are not being used
// and returns ok=false.
// Other Postgres env vars are optional and have sensible defaults.
func getPostgresDSN() (string, bool, error) {
	host := os.Getenv(PostgresHostEnvVar)
	if host == "" {
		return "", false, nil
	}
	port := os.Getenv(PostgresPortEnvVar)
	if port == "" {
		port = "5432"
	}
	dbName, err := getEnvOrFile(PostgresDBEnvVar)
	if err != nil {
		return "", false, fmt.Errorf("failed to get postgres DB name: %w", err)
	}
	if dbName == "" {
		dbName = "postgres"
	}
	pgUser, err := getEnvOrFile(PostgresUserEnvVar)
	if err != nil {
		return "", false, fmt.Errorf("failed to get postgres user: %w", err)
	}
	if pgUser == "" {
		pgUser = "postgres"
	}
	password, err := getEnvOrFile(PostgresPasswordEnvVar)
	if err != nil {
		return "", false, fmt.Errorf("failed to get postgres password: %w", err)
	}
	// password can be empty, so no default value

	// todo: support sslmode param in the dsn constructed here
	dsn := fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s",
		url.QueryEscape(pgUser),
		url.QueryEscape(password),
		host,
		port,
		url.QueryEscape(dbName),
	)

	return dsn, true, nil
}

// getMcpServerInitReqTimeout returns the timeout (in seconds) for MCP server initialization requests.
// If the corresponding environment variable is not set, it returns the default value.
// If the value is invalid, it returns an error.
func getMcpServerInitReqTimeout() (int, error) {
	timeoutStr := strings.TrimSpace(os.Getenv(McpServerInitReqTimeoutSecEnvVar))
	if timeoutStr == "" {
		return McpServerInitRequestTimeoutSecondsDefault, nil
	}
	timeout, err := strconv.Atoi(timeoutStr)
	if err != nil || timeout < 1 {
		return 0, fmt.Errorf(
			"invalid value for %s: '%s', must be a positive integer", McpServerInitReqTimeoutSecEnvVar, timeoutStr,
		)
	}
	return timeout, nil
}

// getSessionIdleTimeout returns the idle timeout (in seconds) for stateful sessions.
func getSessionIdleTimeout() (int, error) {
	timeoutStr := strings.TrimSpace(os.Getenv(SessionIdleTimeoutSecEnvVar))
	if timeoutStr == "" {
		return SessionIdleTimeoutSecondsDefault, nil
	}
	timeout, err := strconv.Atoi(timeoutStr)
	if err != nil || timeout < 0 {
		return 0, fmt.Errorf(
			"invalid value for %s: '%s', must be a non-negative integer (0 = no timeout)",
			SessionIdleTimeoutSecEnvVar, timeoutStr,
		)
	}
	return timeout, nil
}

func runStartServer(cmd *cobra.Command, args []string) error {
	// 启动主链路：读取配置 -> 初始化监控 -> 连接数据库并迁移 -> 创建领域服务
	// -> 注册 HTTP/MCP 路由 -> 校验运行模式 -> 启动监听并处理优雅退出。
	_ = godotenv.Load()

	desiredServerMode, err := getDesiredServerMode(cmd)
	if err != nil {
		return err
	}

	// 先初始化 Telemetry Provider，后续 Gin 和 MCP 调用共享同一套 Meter。
	telemetryEnabled, err := isTelemetryEnabled(desiredServerMode)
	if err != nil {
		return err
	}
	otelConfig := &telemetry.Config{
		ServiceName: "mcp-gateway",
		Enabled:     telemetryEnabled,
	}
	otelProviders, err := telemetry.Init(cmd.Context(), otelConfig)
	if err != nil {
		return fmt.Errorf("failed to initialize Opentelemetry providers: %v", err)
	}
	defer func() {
		if err := otelProviders.Shutdown(cmd.Context()); err != nil {
			cmd.Printf("Warning: failed to shutdown opentelemetry providers: %v\n", err)
		}
	}()

	// 默认注入 no-op metrics；开启监控时才替换成 OpenTelemetry 实现。
	// 这样业务代码始终调用同一接口，无需到处判断开关，也不会出现 nil 指针。
	mcpMetrics := telemetry.NewNoopCustomMetrics()
	if otelProviders.IsEnabled() {
		mcpMetrics, err = telemetry.NewOtelCustomMetrics(otelProviders.Meter)
		if err != nil {
			return fmt.Errorf("failed to create MCP metrics: %v", err)
		}
	}

	// DATABASE_URL 优先；未提供时尝试拼装 PostgreSQL 配置，最终回退到 SQLite。
	dsn := os.Getenv(DBUrlEnvVar)

	if dsn == "" {
		// 只有设置了 POSTGRES_HOST 才认为用户希望使用分项 PostgreSQL 配置。
		pgDSN, ok, err := getPostgresDSN()
		if err != nil {
			return fmt.Errorf("failed to get postgres DSN: %w", err)
		}
		if ok {
			dsn = pgDSN
		}
	}

	dbConn, err := db.NewDBConnection(dsn, getSQLiteDBPathOverride())
	if err != nil {
		return err
	}
	// 迁移理想情况下应作为独立运维步骤；当前为方便本地使用，在启动时自动执行。
	if err := migrations.Migrate(dbConn); err != nil {
		return fmt.Errorf("failed to run migrations: %v", err)
	}

	bindPort := getBindPort()
	bindAddr := net.JoinHostPort(getBindHost(cmd), bindPort)

	mcpProxyServer, sseMcpProxyServer := newProxyServers()

	timeout, err := getMcpServerInitReqTimeout()
	if err != nil {
		return err
	}
	log.Printf("[server] timeout for initialization requests to MCP servers is %d seconds\n", timeout)

	sessionIdleTimeout, err := getSessionIdleTimeout()
	if err != nil {
		return err
	}
	if sessionIdleTimeout > 0 {
		log.Printf("[server] idle timeout for stateful sessions is %d seconds\n", sessionIdleTimeout)
	} else if sessionIdleTimeout == 0 {
		log.Printf("[server] stateful sessions will not timeout (run until server shutdown)\n")
	}

	// Stateful 上游连接统一交给 SessionManager 复用和回收。
	sessionManager := mcp.NewSessionManager(&mcp.SessionManagerConfig{
		DB:                dbConn,
		IdleTimeoutSec:    sessionIdleTimeout,
		InitReqTimeoutSec: timeout,
	})

	mcpServiceConfig := &mcp.ServiceConfig{
		DB:                      dbConn,
		McpProxyServer:          mcpProxyServer,
		SseMcpProxyServer:       sseMcpProxyServer,
		Metrics:                 mcpMetrics,
		McpServerInitReqTimeout: timeout,
		SessionManager:          sessionManager,
	}
	mcpService, err := mcp.NewMCPService(mcpServiceConfig)
	if err != nil {
		return fmt.Errorf("failed to create MCP service: %v", err)
	}

	mcpClientService := mcpclient.NewMCPClientService(dbConn)

	configService := config.NewServerConfigService(dbConn)
	userService := user.NewUserService(dbConn)
	dashboardService := dashboard.NewService(dbConn, otelProviders.IsEnabled())

	toolGroupService, err := toolgroup.NewToolGroupService(dbConn, mcpService)
	if err != nil {
		return fmt.Errorf("failed to create Tool Group service: %v", err)
	}

	// 将已创建的领域服务注入 API 层；API 层只负责编排路由和请求转换。
	opts := &api.ServerOptions{
		MCPProxyServer:    mcpProxyServer,
		SseMcpProxyServer: sseMcpProxyServer,
		MCPService:        mcpService,
		MCPClientService:  mcpClientService,
		ConfigService:     configService,
		UserService:       userService,
		ToolGroupService:  toolGroupService,
		DashboardService:  dashboardService,
		OtelProviders:     otelProviders,
		Metrics:           mcpMetrics,
	}
	s, err := api.NewServer(opts)
	if err != nil {
		return fmt.Errorf("failed to create server: %v", err)
	}

	// 已初始化网关不能临时切换模式，否则现有用户、Token 和权限数据语义可能失配。
	ok, err := s.IsInitialized()
	if err != nil {
		return fmt.Errorf("failed to check if server is initialized: %v", err)
	}
	if ok {
		// 当前启动参数必须与数据库中已经保存的模式一致。
		mode, err := s.GetMode()
		if err != nil {
			return fmt.Errorf("failed to get server mode: %v", err)
		}
		if desiredServerMode != mode {
			return fmt.Errorf(
				"server is already initialized in %s mode, cannot start in %s mode",
				mode, desiredServerMode,
			)
		}
	} else {
		// Dev 面向个人本地使用，首次启动自动完成初始化。
		if desiredServerMode == model.ModeDev {
			if err := s.InitDev(); err != nil {
				return fmt.Errorf("failed to initialize server in development mode: %v", err)
			}
		} else {
			// Enterprise 要求手动初始化，以便操作者安全地取得首次管理员 Token。
			cmd.Println(
				"Starting server in Enterprise mode," +
					" don't forget to initialize it by running the `init-server` command",
			)
		}
	}

	// 到这里依赖和模式检查都已通过，可以展示监听地址。
	cmd.Print(asciiArt)
	cmd.Printf("MCP Gateway HTTP server listening on %s\n\n", bindAddr)

	// 所有请求共享可取消的根 Context，关闭服务时可通知仍在运行的调用退出。
	requestBaseCtx, cancelRequests := context.WithCancel(context.Background())

	// 显式创建 http.Server，而不是直接调用 Gin.Run，目的是支持优雅关闭。
	httpServer := &http.Server{
		Addr:    bindAddr,
		Handler: s.Router(),
		BaseContext: func(l net.Listener) context.Context {
			return requestBaseCtx
		},
	}

	// HTTP Shutdown 触发后取消请求根 Context。
	httpServer.RegisterOnShutdown(func() {
		log.Println("[server] Cancelling active connections...")
		cancelRequests()
	})

	// 监听 Ctrl+C 和容器常用的 SIGTERM。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// 监听放入协程，主协程留在下面等待退出信号。
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("failed to run the server: %v", err)
		}
	}()

	// 阻塞到操作系统通知进程退出。
	sig := <-quit
	log.Printf("[server] Received signal %v, initiating graceful shutdown...\n", sig)

	// 先停止 MCP 会话，避免 HTTP 已关闭但上游连接仍然存活。
	mcpService.Shutdown()

	// 最多等待 10 秒让正在处理的 HTTP 请求结束。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server forced to shutdown: %v", err)
	}

	log.Println("[server] Server gracefully stopped")
	return nil
}
