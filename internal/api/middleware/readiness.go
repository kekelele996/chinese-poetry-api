package middleware

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/palemoky/chinese-poetry-api/internal/health"
)

// retryAfterSeconds 提示负载均衡（与偶尔绕过 LB 直连的客户端）
// 多少秒后再重试，语义与 RFC 9110 的 Retry-After 一致。
const retryAfterSeconds = 5

// ReadinessGate 是业务路由的就绪门控：
//
// 启动阶段（starting）实例尚未完成数据迁移等初始化，
// 摘流阶段（draining）实例正在等待在途请求收尾，
// 这两个窗口内即便有请求绕过编排系统直达端口，也一律拒绝，
// 避免“第一条请求赶上数据还没准备好”或退出期间新请求被强制断开。
//
// 探针路由（/livez、/readyz）不得挂这个中间件。
func ReadinessGate(checker *health.Checker) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !checker.IsReady() {
			c.Header("Retry-After", strconv.Itoa(retryAfterSeconds))
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error":  "service not ready",
				"reason": checker.Phase().String(),
			})
			return
		}

		c.Next()
	}
}
