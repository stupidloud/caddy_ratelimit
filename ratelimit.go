// Package ratelimit 提供基于X-Accel头的动态带宽限速功能
package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// 定义上下文键，用于在请求上下文中存储令牌桶
type contextKey string
const tokenBucketKey contextKey = "token_bucket"

// 定义日志字段键，这些将在整个包中共享
var (
	logKeyUserID = "userID"
	logKeyRate   = "rate"
	logKeyOldRate = "oldRate"
	logKeyNewRate = "newRate"
	logKeyTokens = "tokens"
	logKeyCount = "count"
	logKeyElapsed = "elapsed"
	logKeyNewTokens = "newTokens"
	logKeyTotalTokens = "totalTokens"
	logKeyRequestedCount = "requestedCount"
	logKeyMaxTokens = "maxTokens"
	logKeyRemainingTokens = "remainingTokens"
)

func init() {
	caddy.RegisterModule(RateLimit{})
	// 确保注册为有序的 HTTP 处理器
	httpcaddyfile.RegisterHandlerDirective("rate_limit_dynamic", parseCaddyfile)
	// 设置指令顺序，确保在 reverse_proxy 之前运行
	httpcaddyfile.RegisterDirectiveOrder("rate_limit_dynamic", httpcaddyfile.Before, "reverse_proxy")
}

// RateLimit 实现带宽限速中间件
type RateLimit struct {
	// 用户ID响应头
	HeaderUserID string `json:"header_user_id,omitempty"`

	// 限速值响应头
	HeaderRateLimit string `json:"header_rate_limit,omitempty"`

	// 突发倍数，默认为1
	BurstMultiplier float64 `json:"burst_multiplier,omitempty"`

	// Redis连接字符串，如果为空则使用内存模式
	Redis string `json:"redis,omitempty"`

	// 内部状态
	limiters      map[string]*TokenBucket
	limitersMutex sync.RWMutex
	storage       Storage
	logger        *zap.Logger
	cleanupTicker *time.Ticker
	cleanupDone   chan struct{}
	next          caddyhttp.Handler
}

// CaddyModule 返回Caddy模块信息
func (RateLimit) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.rate_limit_dynamic", // 遵循 http.handlers.<n> 格式
		New: func() caddy.Module { return new(RateLimit) },
	}
}

// Provision 实现caddy.Provisioner接口
func (rl *RateLimit) Provision(ctx caddy.Context) error {
	rl.logger = ctx.Logger(rl)
	rl.limiters = make(map[string]*TokenBucket)
	rl.cleanupDone = make(chan struct{})

	// 设置默认值
	if rl.HeaderUserID == "" {
		rl.HeaderUserID = "X-Accel-User-ID"
	}
	if rl.HeaderRateLimit == "" {
		rl.HeaderRateLimit = "X-Accel-RateLimit"
	}
	if rl.BurstMultiplier <= 0 {
		rl.BurstMultiplier = 1.0
	}

	// 根据配置选择存储后端
	var err error
	if rl.Redis != "" {
		rl.storage, err = NewRedisStorage(rl.Redis, rl.logger)
	} else {
		rl.storage, err = NewMemoryStorage(rl.logger)
	}

	if err != nil {
		return err
	}

	// 启动清理过期限速器的定时任务
	rl.cleanupTicker = time.NewTicker(5 * time.Minute)
	go rl.cleanupExpiredLimiters()

	return nil
}

// Cleanup 实现caddy.CleanerUpper接口
func (rl *RateLimit) Cleanup() error {
	if rl.cleanupTicker != nil {
		rl.cleanupTicker.Stop()
	}
	
	if rl.cleanupDone != nil {
		close(rl.cleanupDone)
	}
	
	// 关闭存储
	if rl.storage != nil {
		if err := rl.storage.Close(); err != nil {
			rl.logger.Error("关闭存储失败", zap.Error(err))
		}
	}
	
	return nil
}

// Validate 实现caddy.Validator接口
func (rl *RateLimit) Validate() error {
	if rl.HeaderUserID == "" {
		return fmt.Errorf("header_user_id不能为空")
	}
	if rl.HeaderRateLimit == "" {
		return fmt.Errorf("header_rate_limit不能为空")
	}
	return nil
}

// ServeHTTP 实现caddyhttp.MiddlewareHandler接口
// 这个方法现在负责处理响应头中的X-Accel-Redirect，并将令牌桶存储在请求上下文中
func (rl *RateLimit) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	rl.next = next
	// 记录请求信息
	rl.logger.Debug("处理请求", 
		zap.String("method", r.Method), 
		zap.String("path", r.URL.Path),
		zap.String("remoteAddr", r.RemoteAddr))

	// 创建一个响应捕获器，用于拦截和检查响应头
	crw := &captureResponseWriter{
		ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: w},
		statusCode:            http.StatusOK,
		header:                make(http.Header),
	}

	// 处理请求，捕获响应
	err := next.ServeHTTP(crw, r)
	if err != nil {
		rl.logger.Error("处理请求失败", zap.Error(err))
		return err
	}

	// 检查必要的头信息是否都存在
	accelRedirect := crw.Header().Get("X-Accel-Redirect")
	userID := crw.Header().Get(rl.HeaderUserID)
	rateLimitStr := crw.Header().Get(rl.HeaderRateLimit)
	
	// 如果任何必要的头信息缺失，则跳过限速处理，但仍需处理内部重定向
	if accelRedirect == "" {
		// 如果没有 X-Accel-Redirect 头，直接返回原始响应
		return nil
	}
	
	// 创建一个新的请求，用于内部重定向
	ctx := r.Context()
	
	// 只有当所有必要的限速信息都存在时，才应用限速
	if userID != "" && rateLimitStr != "" {
		// 解析限速值
		rateLimit, err := strconv.ParseInt(rateLimitStr, 10, 64)
		if err != nil {
			rl.logger.Warn("解析限速值失败", zap.String("value", rateLimitStr), zap.Error(err))
		} else {
			// 使用条件日志记录限速参数
			if rl.logger.Core().Enabled(zapcore.DebugLevel) {
				rl.logger.Debug("获取限速参数", 
					zap.String(logKeyUserID, userID), 
					zap.Int64(logKeyRate, rateLimit),
					zap.String("redirect", accelRedirect))
			}
			
			// 获取或创建令牌桶
			bucket, err := rl.getOrCreateBucket(userID, rateLimit)
			if err != nil {
				rl.logger.Error("获取令牌桶失败", zap.Error(err))
			} else {
				// 将令牌桶存储在请求上下文中，供后续中间件使用
				ctx = context.WithValue(ctx, tokenBucketKey, bucket)
			}
		}
	} else {
		// 记录缺少限速信息的情况
		if rl.logger.Core().Enabled(zapcore.DebugLevel) {
			rl.logger.Debug("缺少限速信息，仅执行内部重定向", 
				zap.String("path", accelRedirect),
				zap.Bool("missingUserID", userID == ""),
				zap.Bool("missingRateLimit", rateLimitStr == ""))
		}
	}
	
	// 执行内部重定向
	if rl.logger.Core().Enabled(zapcore.DebugLevel) {
		rl.logger.Debug("执行内部重定向", zap.String("path", accelRedirect))
	}
	
	// 创建一个新的请求，保留原始请求的上下文（包含令牌桶）
	newReq := r.Clone(ctx)
	newReq.URL.Path = accelRedirect
	newReq.URL.RawPath = ""
	newReq.RequestURI = accelRedirect
	
	// 执行内部重定向
	return rl.next.ServeHTTP(w, newReq)
}

// 获取或创建令牌桶
func (rl *RateLimit) getOrCreateBucket(userID string, rateLimit int64) (*TokenBucket, error) {
	rl.limitersMutex.RLock()
	bucket, exists := rl.limiters[userID]
	rl.limitersMutex.RUnlock()

	if exists {
		// 如果限速值变化，更新令牌桶
		if bucket.Rate() != rateLimit {
			oldRate := bucket.Rate()
			bucket.SetRate(rateLimit)
			
			// 使用条件日志
			if rl.logger.Core().Enabled(zapcore.DebugLevel) {
				rl.logger.Debug("更新令牌桶速率", 
					zap.String(logKeyUserID, userID), 
					zap.Int64(logKeyOldRate, oldRate), 
					zap.Int64(logKeyNewRate, rateLimit))
			}
		}
		return bucket, nil
	}

	// 创建新的令牌桶
	rl.limitersMutex.Lock()
	defer rl.limitersMutex.Unlock()

	// 双重检查，避免并发创建
	if bucket, exists = rl.limiters[userID]; exists {
		if bucket.Rate() != rateLimit {
			oldRate := bucket.Rate()
			bucket.SetRate(rateLimit)
			
			// 使用条件日志
			if rl.logger.Core().Enabled(zapcore.DebugLevel) {
				rl.logger.Debug("并发更新令牌桶速率", 
					zap.String(logKeyUserID, userID), 
					zap.Int64(logKeyOldRate, oldRate), 
					zap.Int64(logKeyNewRate, rateLimit))
			}
		}
		return bucket, nil
	}

	// 创建新的令牌桶
	bucket = NewTokenBucket(rateLimit, rl.storage, userID, rl.logger, rl.BurstMultiplier)
	rl.limiters[userID] = bucket
	
	return bucket, nil
}

// 清理过期的限速器
func (rl *RateLimit) cleanupExpiredLimiters() {
	for {
		select {
		case <-rl.cleanupTicker.C:
			rl.limitersMutex.Lock()
			for userID, bucket := range rl.limiters {
				if time.Since(bucket.LastAccess()) > 30*time.Minute {
					delete(rl.limiters, userID)
					
					// 使用条件日志
					if rl.logger.Core().Enabled(zapcore.DebugLevel) {
						rl.logger.Debug("清理过期令牌桶", zap.String(logKeyUserID, userID))
					}
				}
			}
			rl.limitersMutex.Unlock()
		case <-rl.cleanupDone:
			return
		}
	}
}

// captureResponseWriter 是一个响应写入器包装器，用于捕获响应头和状态码
type captureResponseWriter struct {
	*caddyhttp.ResponseWriterWrapper
	statusCode int
	header     http.Header
	wroteHeader bool
}

// WriteHeader 实现http.ResponseWriter接口
func (crw *captureResponseWriter) WriteHeader(statusCode int) {
	if !crw.wroteHeader {
		crw.statusCode = statusCode
		crw.wroteHeader = true
	}
}

// Header 实现http.ResponseWriter接口
func (crw *captureResponseWriter) Header() http.Header {
	return crw.header
}

// Write 实现http.ResponseWriter接口
func (crw *captureResponseWriter) Write(b []byte) (int, error) {
	if !crw.wroteHeader {
		crw.WriteHeader(http.StatusOK)
	}
	return crw.ResponseWriter.Write(b)
}

// GetTokenBucketFromContext 从请求上下文中获取令牌桶
func GetTokenBucketFromContext(r *http.Request) *TokenBucket {
	if bucket, ok := r.Context().Value(tokenBucketKey).(*TokenBucket); ok {
		return bucket
	}
	return nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*RateLimit)(nil)
	_ caddy.Validator             = (*RateLimit)(nil)
	_ caddyhttp.MiddlewareHandler = (*RateLimit)(nil)
	_ caddy.CleanerUpper          = (*RateLimit)(nil)
)
