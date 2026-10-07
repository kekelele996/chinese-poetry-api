// Package health 维护进程的生命周期阶段，并提供存活与就绪两类探针。
//
// 两个探针有意分开：
//
//   - 存活探针（/livez）只回答“进程还活着、HTTP 还能响应”。
//     初始化（如下载后的数据迁移）耗时再久也不应返回失败，
//     否则编排系统会在启动阶段反复杀掉实例，形成永远起不来的死循环。
//   - 就绪探针（/readyz）回答“实例现在能不能接流量”。
//     启动未完成与退出摘流阶段都返回 503，负载均衡据此不把请求路由过来。
package health

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// Phase 表示实例当前所处的生命周期阶段。
type Phase int32

const (
	// PhaseStarting 已开始启动但初始化尚未完成，不能接业务流量。
	PhaseStarting Phase = iota
	// PhaseReady 初始化全部完成，可以正常接业务流量。
	PhaseReady
	// PhaseDraining 已收到终止信号并从流量列表摘除，等待在途请求结束。
	PhaseDraining
)

// String 返回阶段的可读名称，用于探针响应体与日志。
func (p Phase) String() string {
	switch p {
	case PhaseStarting:
		return "starting"
	case PhaseReady:
		return "ready"
	case PhaseDraining:
		return "draining"
	default:
		return "unknown"
	}
}

// Pinger 用于就绪探针探活依赖（底层 *sql.DB 天然满足该接口）。
type Pinger interface {
	PingContext(ctx context.Context) error
}

// readinessPingTimeout 限制单次就绪探活的最长耗时，
// 避免数据库卡住时探针 goroutine 长时间堆积。
const readinessPingTimeout = 2 * time.Second

// Checker 以原子方式记录生命周期阶段，供探针与就绪门控中间件并发读取。
type Checker struct {
	phase       atomic.Int32
	pinger      Pinger
	pingTimeout time.Duration
}

// NewChecker 创建处于 starting 阶段的检查器。
// pinger 可为 nil（例如没有数据库依赖的场景），此时就绪只取决于阶段。
func NewChecker(pinger Pinger) *Checker {
	c := &Checker{
		pinger:      pinger,
		pingTimeout: readinessPingTimeout,
	}
	c.phase.Store(int32(PhaseStarting))
	return c
}

// Phase 返回当前阶段。
func (c *Checker) Phase() Phase {
	return Phase(c.phase.Load())
}

// MarkReady 声明初始化完成、实例可以接流量。
func (c *Checker) MarkReady() {
	c.phase.Store(int32(PhaseReady))
}

// MarkDraining 声明实例开始退出、不再接新流量。
func (c *Checker) MarkDraining() {
	c.phase.Store(int32(PhaseDraining))
}

// IsReady 在且仅在阶段为 ready 时返回 true。
func (c *Checker) IsReady() bool {
	return c.Phase() == PhaseReady
}

// LivenessHandler 处理存活探针：
// HTTP 服务本身能响应即说明进程存活，因此始终返回 200。
func (c *Checker) LivenessHandler() gin.HandlerFunc {
	return func(c2 *gin.Context) {
		c2.JSON(http.StatusOK, gin.H{
			"status": "alive",
			"phase":  c.Phase().String(),
		})
	}
}

// ReadinessHandler 处理就绪探针：
//   - starting / draining 阶段直接返回 503，不访问数据库；
//   - ready 阶段再 ping 一次数据库，依赖异常时同样返回 503。
func (c *Checker) ReadinessHandler() gin.HandlerFunc {
	return func(c2 *gin.Context) {
		switch phase := c.Phase(); phase {
		case PhaseStarting, PhaseDraining:
			c2.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "not_ready",
				"reason": phase.String(),
			})
			return
		}

		if c.pinger != nil {
			ctx, cancel := context.WithTimeout(c2.Request.Context(), c.pingTimeout)
			err := c.pinger.PingContext(ctx)
			cancel()
			if err != nil {
				c2.JSON(http.StatusServiceUnavailable, gin.H{
					"status": "not_ready",
					"reason": "dependency_unavailable",
				})
				return
			}
		}

		c2.JSON(http.StatusOK, gin.H{
			"status": "ready",
		})
	}
}
