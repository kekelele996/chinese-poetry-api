package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubPinger 模拟依赖探活，可配置返回错误与阻塞行为。
type stubPinger struct {
	err      error
	calls    atomic.Int32
	blockCtx bool // true 时阻塞到 ctx 被取消并返回 ctx.Err()
}

func (s *stubPinger) PingContext(ctx context.Context) error {
	s.calls.Add(1)

	if s.blockCtx {
		<-ctx.Done()
		return ctx.Err()
	}

	return s.err
}

func init() {
	gin.SetMode(gin.TestMode)
}

// serve 用 httptest 执行一次处理器并记录响应。
func serve(handler gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	handler(c)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body
}

func TestLivenessAlwaysOK(t *testing.T) {
	t.Parallel()

	pinger := &stubPinger{}
	checker := NewChecker(pinger)

	for _, phase := range []Phase{PhaseStarting, PhaseReady, PhaseDraining} {
		switch phase {
		case PhaseReady:
			checker.MarkReady()
		case PhaseDraining:
			checker.MarkDraining()
		}

		w := serve(checker.LivenessHandler())
		assert.Equal(t, http.StatusOK, w.Code, "phase=%s", phase)
		body := decodeBody(t, w)
		assert.Equal(t, "alive", body["status"])
	}

	// 存活探针不应触碰依赖
	assert.Zero(t, pinger.calls.Load())
}

func TestReadinessStarting(t *testing.T) {
	t.Parallel()

	pinger := &stubPinger{err: errors.New("db down")}
	checker := NewChecker(pinger)

	w := serve(checker.ReadinessHandler())
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "not_ready", body["status"])
	assert.Equal(t, "starting", body["reason"])

	// 启动阶段不探活依赖，依赖即使异常也不影响判定
	assert.Zero(t, pinger.calls.Load())
}

func TestReadinessReady(t *testing.T) {
	t.Parallel()

	t.Run("healthy dependency", func(t *testing.T) {
		checker := NewChecker(&stubPinger{})
		checker.MarkReady()

		w := serve(checker.ReadinessHandler())
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "ready", decodeBody(t, w)["status"])
	})

	t.Run("nil pinger only checks phase", func(t *testing.T) {
		checker := NewChecker(nil)
		checker.MarkReady()

		w := serve(checker.ReadinessHandler())
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("dependency unavailable", func(t *testing.T) {
		checker := NewChecker(&stubPinger{err: errors.New("connection refused")})
		checker.MarkReady()

		w := serve(checker.ReadinessHandler())
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		body := decodeBody(t, w)
		assert.Equal(t, "dependency_unavailable", body["reason"])
	})

	t.Run("ping timeout", func(t *testing.T) {
		checker := NewChecker(&stubPinger{blockCtx: true})
		checker.pingTimeout = 20 * time.Millisecond
		checker.MarkReady()

		start := time.Now()
		w := serve(checker.ReadinessHandler())
		elapsed := time.Since(start)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Less(t, elapsed, time.Second, "probe should abort at ping timeout")
	})
}

func TestReadinessDraining(t *testing.T) {
	t.Parallel()

	pinger := &stubPinger{}
	checker := NewChecker(pinger)
	checker.MarkReady()
	checker.MarkDraining()

	w := serve(checker.ReadinessHandler())
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "draining", body["reason"])

	// 摘流阶段不再访问依赖，避免无谓的探活开销
	assert.Zero(t, pinger.calls.Load())
}

func TestPhaseTransitions(t *testing.T) {
	t.Parallel()

	checker := NewChecker(nil)
	assert.Equal(t, PhaseStarting, checker.Phase())
	assert.False(t, checker.IsReady())

	checker.MarkReady()
	assert.Equal(t, PhaseReady, checker.Phase())
	assert.True(t, checker.IsReady())

	checker.MarkDraining()
	assert.Equal(t, PhaseDraining, checker.Phase())
	assert.False(t, checker.IsReady())
}
