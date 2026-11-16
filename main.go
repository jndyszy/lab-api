// lab-api：阶段 2 的示例服务，用于演示 CI/CD、GitOps 与阶段 3 的 SLO 监控。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// version 在构建时通过 -ldflags "-X main.version=..." 注入（CI 中为 git commit）
var version = "dev"

var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "HTTP 请求总数，按路径和状态码区分",
	}, []string{"path", "code"})
	duration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP 请求耗时",
		Buckets: prometheus.DefBuckets,
	}, []string{"path"})
)

// config 故障注入参数，用于演示 SLO 错误预算消耗与告警（阶段 3）
type config struct {
	failRate float64       // FAIL_RATE：/ 返回 500 的比例，0~1
	latency  time.Duration // LATENCY_MS：/ 额外增加的延迟
}

func loadConfig() config {
	c := config{}
	if v, err := strconv.ParseFloat(getenv("FAIL_RATE", "0"), 64); err == nil && v >= 0 && v <= 1 {
		c.failRate = v
	}
	if v, err := strconv.Atoi(getenv("LATENCY_MS", "0")); err == nil && v >= 0 {
		c.latency = time.Duration(v) * time.Millisecond
	}
	return c
}

// statusRecorder 记录 handler 写出的状态码，供指标使用
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// instrument 给业务接口加上请求计数、耗时统计和访问日志
// 只包装业务接口；探针和 /metrics 不记访问日志，避免每 2 秒一条的噪音
func instrument(path string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		h(rec, r)
		elapsed := time.Since(start)
		duration.WithLabelValues(path).Observe(elapsed.Seconds())
		requests.WithLabelValues(path, strconv.Itoa(rec.code)).Inc()

		// 结构化访问日志：每个字段都是独立的 JSON 键，Loki 中可用 | json 直接按字段过滤
		level := slog.LevelInfo
		if rec.code >= 500 {
			level = slog.LevelError
		}
		slog.LogAttrs(r.Context(), level, "request",
			slog.String("method", r.Method),
			slog.String("path", path),
			slog.Int("status", rec.code),
			slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
			slog.String("remote_addr", r.RemoteAddr),
		)
	}
}

// newMux 组装所有路由；ready 为 false 时就绪探针返回 503
func newMux(cfg config, hostname string, ready *atomic.Bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", instrument("/", func(w http.ResponseWriter, r *http.Request) {
		if cfg.latency > 0 {
			time.Sleep(cfg.latency)
		}
		if cfg.failRate > 0 && rand.Float64() < cfg.failRate {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"service": "lab-api", "version": version, "hostname": hostname,
		})
	}))
	// 存活探针：进程能响应就返回 200
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	// 就绪探针：收到 SIGTERM 后返回 503，让 Service 先摘掉本 Pod 再退出
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ready"))
	})
	mux.Handle("GET /metrics", promhttp.Handler())
	return mux
}

func main() {
	// 日志统一输出 JSON 到 stdout，由节点上的采集器（Alloy）收集到 Loki
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "lab-api", "version", version))
	cfg := loadConfig()
	hostname, _ := os.Hostname()
	var ready atomic.Bool
	ready.Store(true)

	// 预先创建成功和失败两个时间序列（初始值 0），否则某个副本从没出过 500 时，
	// 错误率查询里会缺这个序列
	requests.WithLabelValues("/", "200")
	requests.WithLabelValues("/", "500")

	addr := ":" + getenv("PORT", "8080")
	srv := &http.Server{Addr: addr, Handler: newMux(cfg, hostname, &ready), ReadHeaderTimeout: 5 * time.Second}

	go func() {
		slog.Info("listening", "addr", addr, "fail_rate", cfg.failRate, "latency_ms", cfg.latency.Milliseconds())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	// 优雅退出：先标记未就绪，等 Endpoint 摘除生效，再停止接收新连接并处理完存量请求
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	slog.Info("received shutdown signal")
	ready.Store(false)
	time.Sleep(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "err", err)
	}
	slog.Info("bye")
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
