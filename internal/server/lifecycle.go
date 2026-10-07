// Package server 编排 HTTP 服务的启动与优雅退出。
//
// 退出分三步，对应滚动更新时的典型时序：
//
//  1. 标记 draining，就绪探针随即返回 503，实例被移出流量列表；
//  2. 等待一个短暂的排空延迟（drain delay），给负载均衡留出收敛时间，
//     期间不再接受新业务请求（由就绪门控兜底），在途请求继续执行；
//  3. 关闭监听器并等待在途请求自然结束，超过 shutdown timeout 才强制收尾。
//
// 排空延迟或等待期间再次收到终止信号，则不等窗口立即强制关闭。
package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/palemoky/chinese-poetry-api/internal/health"
	"github.com/palemoky/chinese-poetry-api/internal/logger"
)

// Lifecycle 绑定一个 HTTP 服务与它的生命周期状态。
type Lifecycle struct {
	Server          *http.Server
	Health          *health.Checker
	DrainDelay      time.Duration // 标记摘流后、关闭监听器前的等待，留给 LB 收敛
	ShutdownTimeout time.Duration // 关闭监听器后等待在途请求的最长时间
}

// Serve 在后台启动 HTTP 服务，返回的 channel 只会传送一次启动错误。
//
// 监听器在返回前就完成绑定，因此端口占用等错误会立刻送达 channel，
// 不会与后续初始化混在一起。优雅退出触发的 http.ErrServerClosed
// 不会送入 channel：它是 Shutdown 的正常结果，并非启动失败。
func (l *Lifecycle) Serve() <-chan error {
	ln, err := net.Listen("tcp", l.Server.Addr)
	if err != nil {
		startErr := make(chan error, 1)
		startErr <- err
		return startErr
	}

	return l.ServeOn(ln)
}

// ServeOn 使用给定监听器在后台启动服务，主要用于测试随机端口。
func (l *Lifecycle) ServeOn(ln net.Listener) <-chan error {
	startErr := make(chan error, 1)

	go func() {
		if err := l.Server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			startErr <- err
		}
	}()

	return startErr
}

// Drain 执行摘流与优雅关闭。
//
// force 中一旦收到信号（第二次 SIGINT/SIGTERM），排空延迟立即结束、
// Shutdown 上下文立即取消；不传信号则始终等满超时。
//
// 注意 Shutdown 在上下文超时或取消时只是“停止等待”，并不会主动断开
// 仍在执行的连接（Go 标准库要求调用方再 Close 才强收）。因此一旦
// Shutdown 返回错误，这里紧接着调用 Close 强制断开剩余连接，
// 保证“超时再收尾”与“强制停止”都能真正结束在途请求。
//
// 返回值：全部在途请求自然结束时为 nil；超时时为 context.DeadlineExceeded，
// 被强收时为 context.Canceled，调用方只需记录日志。
func (l *Lifecycle) Drain(force <-chan struct{}) error {
	l.Health.MarkDraining()

	if l.DrainDelay > 0 {
		l.waitDrainDelay(force)
	}

	ctx, cancel := context.WithTimeout(context.Background(), l.ShutdownTimeout)
	defer cancel()

	// 第二次终止信号：取消 Shutdown 的等待上下文，立即强制关闭。
	stop := make(chan struct{})
	defer close(stop)

	go func() {
		select {
		case <-force:
			cancel()
		case <-stop:
		}
	}()

	err := l.Server.Shutdown(ctx)
	if err == nil {
		return nil
	}

	// Shutdown 未在窗口内结束：强制断开仍存活的连接。
	if closeErr := l.Server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
		logger.Warn("Force-closing remaining connections failed", zap.Error(closeErr))
	}

	if errors.Is(err, context.Canceled) {
		logger.Warn("Shutdown forced, remaining in-flight requests interrupted")
	} else {
		logger.Warn("Shutdown window elapsed, remaining in-flight requests force-closed",
			zap.Error(err))
	}

	return err
}

// waitDrainDelay 等待排空延迟；force 提前触发时立即返回。
func (l *Lifecycle) waitDrainDelay(force <-chan struct{}) {
	timer := time.NewTimer(l.DrainDelay)
	defer timer.Stop()

	select {
	case <-timer.C:
		logger.Info("Drain delay elapsed, closing listener")
	case <-force:
		logger.Warn("Forced shutdown during drain delay, closing listener immediately")
	}
}
