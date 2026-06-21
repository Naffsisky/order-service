package main

import (
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// cpuStressActive adalah flag atomik untuk chaos skenario #4 (CPU stress)
var cpuStressActive atomic.Int32

// leakyStore adalah intentional leak untuk chaos skenario #3 (memory leak)
var leakyStore [][]byte

type OrderRequest struct {
	UserID  string  `json:"userId"  binding:"required"`
	Amount  float64 `json:"amount"  binding:"required,gt=0"`
	Branch  string  `json:"branch"`
	Channel string  `json:"channel"`
}

type OrderResponse struct {
	OrderID   string    `json:"orderId"`
	Status    string    `json:"status"`
	UserID    string    `json:"userId"`
	Amount    float64   `json:"amount"`
	CreatedAt time.Time `json:"createdAt"`
}

func main() {
	// Structured JSON logger via slog — Datadog auto-parse JSON logs
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestLogger())

	// Normal endpoints
	r.GET("/health", healthHandler)
	r.POST("/orders", createOrderHandler)

	// ── Chaos Endpoints (Phase 5) ──────────────────────────────────────────
	// Skenario #1 & #8 — Latency spike
	// Aktifkan: kubectl set env deploy/order-service CHAOS_DELAY_MS=3000
	r.GET("/chaos/slow", func(c *gin.Context) {
		delay, _ := strconv.Atoi(envOr("CHAOS_DELAY_MS", "0"))
		if delay > 0 {
			time.Sleep(time.Duration(delay) * time.Millisecond)
		}
		c.JSON(http.StatusOK, gin.H{"delayed_ms": delay})
	})

	// Skenario #2 — Error burst
	// Aktifkan: kubectl set env deploy/order-service CHAOS_ERROR_RATE=0.8
	r.GET("/chaos/error", func(c *gin.Context) {
		rate, _ := strconv.ParseFloat(envOr("CHAOS_ERROR_RATE", "0"), 64)
		if rate > 0 && rand.Float64() < rate {
			slog.Error("simulated error injected", "event", "chaos_error", "rate", rate)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "simulated failure"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "chaos_error_rate": rate})
	})

	// Skenario #3 — Memory leak → OOMKilled
	// Tiap hit: alokasi 10MB ke leakyStore yang tidak pernah di-GC
	// Pantau di Datadog: container.memory.usage naik bertahap → pod restart
	r.GET("/chaos/memory", func(c *gin.Context) {
		blob := make([]byte, 10*1024*1024) // 10MB per request
		leakyStore = append(leakyStore, blob)
		slog.Warn("memory leak allocated",
			"event", "chaos_memory",
			"allocated_mb", 10,
			"total_slabs", len(leakyStore),
		)
		c.JSON(http.StatusOK, gin.H{
			"allocated_mb": 10,
			"total_slabs":  len(leakyStore),
			"warning":      "memory leak active — pod will OOMKill eventually",
		})
	})

	// Skenario #4 — CPU stress tanpa mengganggu user latency
	// Pelajaran emas: CPU 95% tapi p99 latency normal → alert CPU = noise
	// Aktifkan:  GET /chaos/cpu?action=start
	// Matikan:   GET /chaos/cpu?action=stop
	r.GET("/chaos/cpu", func(c *gin.Context) {
		action := c.Query("action")
		switch action {
		case "start":
			if cpuStressActive.CompareAndSwap(0, 1) {
				go func() {
					slog.Warn("cpu stress goroutine started", "event", "chaos_cpu_start")
					for cpuStressActive.Load() == 1 {
						// busy-loop di goroutine terpisah
						// request handler TIDAK terpengaruh → latency tetap normal
						for i := 0; i < 1_000_000; i++ {
						}
					runtime.Gosched() // yield agar goroutine lain tetap jalan
					}
					slog.Info("cpu stress goroutine stopped", "event", "chaos_cpu_stop")
				}()
				c.JSON(http.StatusOK, gin.H{"cpu_stress": "started"})
			} else {
				c.JSON(http.StatusOK, gin.H{"cpu_stress": "already running"})
			}
		case "stop":
			cpuStressActive.Store(0)
			c.JSON(http.StatusOK, gin.H{"cpu_stress": "stopped"})
		default:
			c.JSON(http.StatusOK, gin.H{
				"cpu_stress_active": cpuStressActive.Load() == 1,
				"usage":             "GET /chaos/cpu?action=start|stop",
			})
		}
	})

	port := envOr("PORT", "8080")
	slog.Info("order-service starting", "port", port, "service", "order-service")
	if err := r.Run(":" + port); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"service": "order-service",
		"version": envOr("DD_VERSION", "1.0.0"),
	})
}

func createOrderHandler(c *gin.Context) {
	var req OrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		slog.Warn("invalid order request", "error", err.Error())
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Branch == "" {
		req.Branch = "jakarta"
	}
	if req.Channel == "" {
		req.Channel = "web"
	}

	order := OrderResponse{
		OrderID:   "ORD-" + uuid.New().String()[:8],
		Status:    "processing",
		UserID:    req.UserID,
		Amount:    req.Amount,
		CreatedAt: time.Now().UTC(),
	}

	// Structured log — tiap field bisa di-query di Datadog Log Explorer
	slog.Info("order created",
		"event", "order_created",
		"orderId", order.OrderID,
		"userId", order.UserID,
		"amount", order.Amount,
		"branch", req.Branch,
		"channel", req.Channel,
		"service", "order-service",
	)

	c.JSON(http.StatusCreated, order)
}

// requestLogger middleware — log setiap request dalam format JSON
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("http_request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP(),
			"service", "order-service",
		)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
