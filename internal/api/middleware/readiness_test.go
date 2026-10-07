package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/palemoky/chinese-poetry-api/internal/health"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newGateRouter 挂载就绪门控，backendCalled 记录门控后的业务处理器是否执行。
func newGateRouter(checker *health.Checker, backendCalled *bool) *gin.Engine {
	router := gin.New()
	router.Use(ReadinessGate(checker))
	router.GET("/work", func(c *gin.Context) {
		*backendCalled = true
		c.String(http.StatusOK, "ok")
	})
	return router
}

func TestReadinessGate(t *testing.T) {
	t.Parallel()

	t.Run("starting rejects business traffic", func(t *testing.T) {
		checker := health.NewChecker(nil)
		called := false
		router := newGateRouter(checker, &called)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/work", nil)
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "5", w.Header().Get("Retry-After"))
		assert.False(t, called, "backend must not run before ready")
	})

	t.Run("ready passes through", func(t *testing.T) {
		checker := health.NewChecker(nil)
		checker.MarkReady()
		called := false
		router := newGateRouter(checker, &called)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/work", nil)
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.True(t, called)
	})

	t.Run("draining rejects new traffic", func(t *testing.T) {
		checker := health.NewChecker(nil)
		checker.MarkReady()
		checker.MarkDraining()
		called := false
		router := newGateRouter(checker, &called)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/work", nil)
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "5", w.Header().Get("Retry-After"))
		assert.False(t, called, "backend must not receive new work while draining")
	})
}
