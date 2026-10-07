package rest

import (
	"github.com/gin-gonic/gin"

	"github.com/palemoky/chinese-poetry-api/internal/api/middleware"
	"github.com/palemoky/chinese-poetry-api/internal/api/rest/handler"
	"github.com/palemoky/chinese-poetry-api/internal/config"
	"github.com/palemoky/chinese-poetry-api/internal/database"
	"github.com/palemoky/chinese-poetry-api/internal/health"
)

// SetupRouter 初始化 Gin 路由并注册全部接口。
//
// 返回的 *middleware.RateLimiter 在限流启用时非空，
// 调用方需在进程退出时调用其 Stop 结束后台回收协程；
// 限流未启用时返回 nil。
//
// 探针路由（/livez、/readyz）不受就绪门控限制，任何阶段都可响应：
//   - /livez 存活：进程能响应即 200；
//   - /readyz 就绪：初始化完成、数据库可达才 200。
//
// 业务路由统一挂就绪门控，starting / draining 阶段返回 503，
// 与编排系统的摘流互为兜底。
func SetupRouter(
	cfg *config.Config,
	db *database.DB,
	repo *database.Repository,
	checker *health.Checker,
) (*gin.Engine, *middleware.RateLimiter) {
	// 设置 Gin 运行模式
	gin.SetMode(cfg.Server.Mode)

	router := gin.New()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	// 跨域中间件
	router.Use(middleware.CORS())

	// 限流中间件
	var rateLimiter *middleware.RateLimiter
	if cfg.RateLimit.Enabled {
		rateLimiter = middleware.NewRateLimiter(cfg.RateLimit.RequestsPerSecond, cfg.RateLimit.Burst)
		router.Use(rateLimiter.Middleware())
	}

	// 探针：Kubernetes 风格的根路径，独立于业务版本前缀与就绪门控
	router.GET("/livez", checker.LivenessHandler())
	router.GET("/readyz", checker.ReadinessHandler())

	// v1 版本接口
	v1 := router.Group("/api/v1")
	{
		// 健康检查（历史路径，语义与 /readyz 保持一致，供旧监控使用）
		v1.GET("/health", checker.ReadinessHandler())

		// 其余业务接口未就绪时一律拒绝
		ready := v1.Group("", middleware.ReadinessGate(checker))

		// 统计数据
		ready.GET("/stats", handler.StatsHandler(repo))

		// 诗词相关
		poemHandler := handler.NewPoemHandler(repo)
		ready.GET("/poems", poemHandler.ListPoems)
		ready.GET("/poems/random", poemHandler.RandomPoem)
		ready.GET("/poems/search", poemHandler.SearchPoems)

		// 作者相关
		authorHandler := handler.NewAuthorHandler(repo)
		ready.GET("/authors", authorHandler.ListAuthors)
		ready.GET("/authors/:id", authorHandler.GetAuthor)

		// 朝代相关
		dynastyHandler := handler.NewDynastyHandler(repo)
		ready.GET("/dynasties", dynastyHandler.ListDynasties)
		ready.GET("/dynasties/:id", dynastyHandler.GetDynasty)

		// 体裁相关
		poetryTypeHandler := handler.NewPoetryTypeHandler(repo)
		ready.GET("/types", poetryTypeHandler.ListPoetryTypes)
		ready.GET("/types/:id", poetryTypeHandler.GetPoetryType)
	}

	return router, rateLimiter
}
