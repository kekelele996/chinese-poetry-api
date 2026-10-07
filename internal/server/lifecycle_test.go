package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/palemoky/chinese-poetry-api/internal/api/middleware"
	"github.com/palemoky/chinese-poetry-api/internal/health"
	"github.com/palemoky/chinese-poetry-api/internal/logger"
)

const testScheme = "http://"

func init() {
	gin.SetMode(gin.TestMode)
	// 预期内的强收告警在生产配置下不打印调用栈，保持测试输出可读
	logger.Init(false)
}

// testEnv 搭起一套带探针与就绪门控的真实 HTTP 服务。
//
// /slow 在收到请求时先通知 entered，阻塞到 release 关闭才返回，
// 用于模拟退出时尚未结束的在途请求。
type testEnv struct {
	baseURL string
	lc      *Lifecycle
	entered chan struct{}
	release chan struct{}
}

func newTestEnv(t *testing.T, drainDelay, shutdownTimeout time.Duration) *testEnv {
	t.Helper()

	checker := health.NewChecker(nil)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})

	router := gin.New()
	router.GET("/livez", checker.LivenessHandler())
	router.GET("/readyz", checker.ReadinessHandler())

	gate := middleware.ReadinessGate(checker)
	router.GET("/slow", gate, func(c *gin.Context) {
		entered <- struct{}{}
		<-release
		c.String(http.StatusOK, "done")
	})
	router.GET("/fast", gate, func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	lc := &Lifecycle{
		Server:          &http.Server{Handler: router},
		Health:          checker,
		DrainDelay:      drainDelay,
		ShutdownTimeout: shutdownTimeout,
	}
	startErr := lc.ServeOn(ln)

	go func() {
		if err := <-startErr; err != nil {
			t.Logf("server error: %v", err)
		}
	}()

	return &testEnv{
		baseURL: testScheme + ln.Addr().String(),
		lc:      lc,
		entered: entered,
		release: release,
	}
}

func TestProbesAndGateBeforeReady(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, time.Millisecond, time.Millisecond)
	t.Cleanup(func() { _ = env.lc.Server.Close() })

	// 存活探针在 starting 阶段就能响应
	resp, err := http.Get(env.baseURL + "/livez")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// 就绪探针返回 503，流量不应被路由过来
	resp, err = http.Get(env.baseURL + "/readyz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	resp.Body.Close()

	// 门控兜底：即便请求直达端口，业务接口也拒绝
	resp, err = http.Get(env.baseURL + "/fast")
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	resp.Body.Close()
}

func TestGracefulDrainWaitsForInflight(t *testing.T) {
	t.Parallel()

	const drainDelay = 500 * time.Millisecond

	env := newTestEnv(t, drainDelay, 2*time.Second)

	env.lc.Health.MarkReady()

	// 就绪后业务可正常访问
	resp, err := http.Get(env.baseURL + "/fast")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// 发起一个会阻塞在服务端的在途请求
	slowStatus := make(chan int, 1)
	go func() {
		r, err := http.Get(env.baseURL + "/slow")
		if err != nil {
			slowStatus <- 0
			return
		}
		slowStatus <- r.StatusCode
		r.Body.Close()
	}()

	select {
	case <-env.entered:
	case <-time.After(time.Second):
		t.Fatal("slow request never reached the handler")
	}

	// 开始摘流
	drainDone := make(chan error, 1)
	go func() { drainDone <- env.lc.Drain(make(chan struct{})) }()

	// 排空延迟期间监听器仍开着，但就绪探针与业务接口都应拒绝新请求。
	// 用轮询代替固定 sleep：观测到 draining 响应即说明摘流已生效，
	// 同时避免在高负载（-race 并行）下误撞上排空窗口结束。
	assert.Eventually(t, func() bool {
		r, err := http.Get(env.baseURL + "/readyz")
		if err != nil {
			return false
		}
		ok := r.StatusCode == http.StatusServiceUnavailable
		r.Body.Close()
		return ok
	}, 200*time.Millisecond, 5*time.Millisecond, "readyz should report draining")

	assert.Eventually(t, func() bool {
		r, err := http.Get(env.baseURL + "/fast")
		if err != nil {
			// 监听器已关，后续也不会再有 503，轮询会自然失败
			return false
		}
		ok := r.StatusCode == http.StatusServiceUnavailable
		r.Body.Close()
		return ok
	}, 200*time.Millisecond, 5*time.Millisecond, "new business request rejected during drain delay")

	// 放行在途请求，它必须拿到 200 而不是被断开
	start := time.Now()
	close(env.release)

	select {
	case status := <-slowStatus:
		assert.Equal(t, http.StatusOK, status, "in-flight request must finish naturally")
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request did not complete after drain")
	}

	select {
	case err := <-drainDone:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Drain did not return")
	}

	// 摘流等待确实发生过，给 LB 留出了收敛窗口
	assert.GreaterOrEqual(t, time.Since(start), drainDelay-50*time.Millisecond)

	// 监听器已关闭，新连接应被拒绝
	_, err = http.Get(env.baseURL + "/livez")
	assert.Error(t, err)
}

func TestForcedShutdownInterruptsInflight(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, 300*time.Millisecond, 5*time.Second)
	env.lc.Health.MarkReady()

	slowErr := make(chan error, 1)
	go func() {
		_, err := http.Get(env.baseURL + "/slow")
		slowErr <- err
	}()

	select {
	case <-env.entered:
	case <-time.After(time.Second):
		t.Fatal("slow request never reached the handler")
	}

	// 100ms 后发出“第二次终止信号”
	force := make(chan struct{})
	time.AfterFunc(100*time.Millisecond, func() { close(force) })

	start := time.Now()
	err := env.lc.Drain(force)
	elapsed := time.Since(start)

	// 排空延迟与在途请求都不再等，立即强收
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, time.Second, "force should bypass drain delay and shutdown timeout")

	select {
	case err := <-slowErr:
		assert.Error(t, err, "in-flight connection must be cut on forced shutdown")
	case <-time.After(time.Second):
		t.Fatal("in-flight request was not interrupted")
	}

	// release 仅用于解除 handler 阻塞，避免 goroutine 泄漏到测试结束
	close(env.release)
}

func TestShutdownTimeoutForceClosesHangingRequest(t *testing.T) {
	t.Parallel()

	// 不触发 force，靠 shutdown timeout 收尾
	env := newTestEnv(t, time.Millisecond, 100*time.Millisecond)
	env.lc.Health.MarkReady()

	go func() {
		_, _ = http.Get(env.baseURL + "/slow")
	}()

	select {
	case <-env.entered:
	case <-time.After(time.Second):
		t.Fatal("slow request never reached the handler")
	}

	err := env.lc.Drain(make(chan struct{}))
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "expected deadline, got %v", err)

	close(env.release)
}
