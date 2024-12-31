package main

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Config 应用配置
type Config struct {
	Rate            rate.Limit    // 每秒允许的请求数
	Burst           int           // 令牌桶的容量
	Port            string        // 服务端口
	CleanupInterval time.Duration // 清理过期IP的间隔
}

// RateLimiter IP限流器
type RateLimiter struct {
	mu    *sync.RWMutex
	ips   map[string]*ipLimiter
	rate  rate.Limit
	burst int
}

type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewRateLimiter 创建一个新的限流器
func NewRateLimiter(rate rate.Limit, burst int) *RateLimiter {
	return &RateLimiter{
		ips:   make(map[string]*ipLimiter),
		mu:    &sync.RWMutex{},
		rate:  rate,
		burst: burst,
	}
}

// AddIP 添加新的IP限流器
func (r *RateLimiter) AddIP(ip string) *rate.Limiter {
	r.mu.Lock()
	defer r.mu.Unlock()

	limiter := &ipLimiter{
		limiter:  rate.NewLimiter(r.rate, r.burst),
		lastSeen: time.Now(),
	}
	r.ips[ip] = limiter
	return limiter.limiter
}

// GetLimiter 获取IP对应的限流器，如果不存在则创建
func (r *RateLimiter) GetLimiter(ip string) *rate.Limiter {
	r.mu.RLock()
	limiter, exists := r.ips[ip]
	r.mu.RUnlock()

	if !exists {
		return r.AddIP(ip)
	}

	r.mu.Lock()
	limiter.lastSeen = time.Now()
	r.mu.Unlock()

	return limiter.limiter
}

// CleanupStaleIPs 清理过期的IP限流器
func (r *RateLimiter) CleanupStaleIPs(maxAge time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for ip, limiter := range r.ips {
		if time.Since(limiter.lastSeen) > maxAge {
			delete(r.ips, ip)
		}
	}
}

var (
	// 默认配置
	config = Config{
		Rate:            1, // 每秒1个请求
		Burst:           5, // 最多允许5个并发请求
		Port:            ":8080",
		CleanupInterval: 5 * time.Minute,
	}
	limiter = NewRateLimiter(config.Rate, config.Burst)
)

func main() {
	// 启动清理过期IP的goroutine
	go func() {
		ticker := time.NewTicker(config.CleanupInterval)
		for range ticker.C {
			limiter.CleanupStaleIPs(config.CleanupInterval)
		}
	}()

	// 设置路由
	mux := http.NewServeMux()
	mux.HandleFunc("/", rateLimitMiddleware(handleRequest))

	server := &http.Server{
		Addr:    config.Port,
		Handler: mux,
	}

	fmt.Printf("Server starting on port %s\n", config.Port)
	if err := server.ListenAndServe(); err != nil {
		fmt.Printf("Server error: %v\n", err)
	}
}

// rateLimitMiddleware 限流中间件
func rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		limiter := limiter.GetLimiter(ip)

		if !limiter.Allow() {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// handleRequest 处理请求的处理器
func handleRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "success", "message": "Hello, World!"}`))
}
