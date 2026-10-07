package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/palemoky/chinese-poetry-api/internal/api/middleware"
	"github.com/palemoky/chinese-poetry-api/internal/api/rest"
	"github.com/palemoky/chinese-poetry-api/internal/config"
	"github.com/palemoky/chinese-poetry-api/internal/database"
	"github.com/palemoky/chinese-poetry-api/internal/graph"
	"github.com/palemoky/chinese-poetry-api/internal/graph/generated"
	"github.com/palemoky/chinese-poetry-api/internal/health"
	"github.com/palemoky/chinese-poetry-api/internal/logger"
	"github.com/palemoky/chinese-poetry-api/internal/server"
)

// graphqlHandler 构造 GraphQL 请求的 Gin handler。
func graphqlHandler(resolver *graph.Resolver) gin.HandlerFunc {
	h := handler.NewDefaultServer(generated.NewExecutableSchema(generated.Config{Resolvers: resolver}))

	return func(c *gin.Context) {
		h.ServeHTTP(c.Writer, c.Request)
	}
}

// playgroundHandler 构造 GraphQL Playground 页面的 Gin handler。
func playgroundHandler() gin.HandlerFunc {
	h := playground.Handler("GraphQL", "/graphql")

	return func(c *gin.Context) {
		h.ServeHTTP(c.Writer, c.Request)
	}
}

func main() {
	// 初始化日志（run 内再 defer Sync，这里先保证加载配置失败也有日志可用）
	debug := os.Getenv("GIN_MODE") != "release"
	logger.Init(debug)

	if err := run(); err != nil {
		logger.Error("Server terminated with error", zap.Error(err))
		logger.Sync()
		os.Exit(1)
	}
}

// run 承载服务的完整生命周期。
//
// 与直接在 main 里调用 logger.Fatal（底层 os.Exit 不会执行 defer）不同，
// 这里以返回错误的方式退出，保证启动失败、正常退出与被强制停止时，
// defer 注册的数据库连接关闭、后台协程停止与日志刷盘都能执行到。
func run() error {
	defer logger.Sync()

	// 加载配置，失败则退回默认配置
	cfg, err := config.Load("config.yaml")
	if err != nil {
		logger.Warn("Failed to load config file, using defaults", zap.Error(err))
		cfg, err = config.Load("")
		if err != nil {
			return fmt.Errorf("load default config: %w", err)
		}
	}

	logger.Info("Starting Chinese Poetry API server",
		zap.String("database", cfg.Database.Path),
		zap.Int("port", cfg.Server.Port),
		zap.Int("max_open_conns", cfg.Database.MaxOpenConns),
		zap.Int("max_idle_conns", cfg.Database.MaxIdleConns),
		zap.Duration("drain_delay", cfg.Server.DrainDelay),
		zap.Duration("shutdown_timeout", cfg.Server.ShutdownTimeout),
	)

	// 按配置的连接池参数打开数据库
	db, err := database.Open(cfg.Database.Path, cfg.Database.MaxOpenConns, cfg.Database.MaxIdleConns)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Warn("Failed to close database", zap.Error(err))
		} else {
			logger.Info("Database connection closed")
		}
	}()

	// 就绪探针需要直接 ping 底层连接池
	sqlDB, err := db.DB.DB()
	if err != nil {
		return fmt.Errorf("get underlying sql.DB: %w", err)
	}

	// 创建仓储与 GraphQL resolver
	repo := database.NewRepository(db)
	resolver := graph.NewResolver(db, repo)

	// 生命周期状态：初始为 starting，探针只放行存活、就绪返回 503
	checker := health.NewChecker(sqlDB)

	// 初始化 Gin 路由
	router, rateLimiter := rest.SetupRouter(cfg, db, repo, checker)
	if rateLimiter != nil {
		defer func() {
			rateLimiter.Stop()
			logger.Info("Rate limiter background worker stopped")
		}()
	}

	// GraphQL 同样受就绪门控，初始化与摘流阶段不接请求
	gate := middleware.ReadinessGate(checker)
	router.POST("/graphql", gate, graphqlHandler(resolver))
	if cfg.GraphQL.Playground {
		router.GET("/playground", gate, playgroundHandler())
		logger.Info("GraphQL Playground enabled", zap.String("path", "/playground"))
	}

	// 构造 HTTP 服务与生命周期编排
	lc := &server.Lifecycle{
		Server: &http.Server{
			Addr:    fmt.Sprintf(":%d", cfg.Server.Port),
			Handler: router,
		},
		Health:          checker,
		DrainDelay:      cfg.Server.DrainDelay,
		ShutdownTimeout: cfg.Server.ShutdownTimeout,
	}

	// 先把监听起来：存活探针立即能响应，避免初始化耗时较长时
	// 被存活探针误杀；就绪探针在此期间始终返回 503。
	startErr := lc.Serve()

	// 补齐数据库结构后再对外提供服务。
	//
	// 服务端只读数据，但仍要跑一次迁移：查询依赖的部分结构（如物化的
	// authors.poem_count 与 counters 计数器）是随版本新增的，老库里并不存在，
	// 跳过迁移只会在第一个请求上报 "no such column"。
	// Migrate 是幂等的，只补缺失的表、列、索引与触发器，不会改动已导入的诗词。
	logger.Info("Initializing database schema...")
	if err := db.Migrate(); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	// 验证数据路径确实可用（表已建好、查询能跑通）后再翻转就绪状态。
	totalPoems, err := repo.CountPoems()
	if err != nil {
		return fmt.Errorf("verify database readiness: %w", err)
	}

	// 端口绑定失败等启动错误会在后台 goroutine 中暴露，就绪前再确认一次
	select {
	case err := <-startErr:
		return fmt.Errorf("http server: %w", err)
	default:
	}

	checker.MarkReady()

	logger.Info("Server is ready to accept traffic",
		zap.Int("port", cfg.Server.Port),
		zap.Int("poems", totalPoems),
		zap.String("rest_api", fmt.Sprintf("http://localhost:%d/api/v1", cfg.Server.Port)),
		zap.String("graphql", fmt.Sprintf("http://localhost:%d/graphql", cfg.Server.Port)),
	)

	// 等待中断信号或服务异常退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("Received termination signal, starting graceful shutdown",
			zap.String("signal", sig.String()),
		)
	case err := <-startErr:
		return fmt.Errorf("http server: %w", err)
	}

	// 再次收到终止信号时立即强制收尾（不等排空延迟与在途请求）
	force := make(chan struct{})
	go func() {
		sig, ok := <-sigCh
		if ok {
			logger.Warn("Received second termination signal, forcing shutdown",
				zap.String("signal", sig.String()),
			)
			close(force)
		}
	}()

	// 摘流 → 排空延迟 → 等待在途请求（超时/二次信号时由 Drain 内部强收）
	if err := lc.Drain(force); err != nil {
		logger.Warn("Graceful shutdown did not finish cleanly", zap.Error(err))
	} else {
		logger.Info("All in-flight requests completed")
	}

	logger.Info("Server exited")
	return nil
}
