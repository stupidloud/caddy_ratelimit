// Package ratelimit 提供基于X-Accel头的动态带宽限速功能
package ratelimit

import (
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(RateLimitInterceptor{})
	// 确保注册为有序的 HTTP 处理器
	httpcaddyfile.RegisterHandlerDirective("rate_limit_interceptor", parseInterceptorCaddyfile)
	// 设置指令顺序，确保在 file_server 之前运行
	httpcaddyfile.RegisterDirectiveOrder("rate_limit_interceptor", httpcaddyfile.Before, "file_server")
}

// RateLimitInterceptor 实现内部重定向限速拦截器
type RateLimitInterceptor struct {
	logger *zap.Logger
}

// CaddyModule 返回Caddy模块信息
func (RateLimitInterceptor) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.rate_limit_interceptor",
		New: func() caddy.Module { return new(RateLimitInterceptor) },
	}
}

// Provision 实现caddy.Provisioner接口
func (rli *RateLimitInterceptor) Provision(ctx caddy.Context) error {
	rli.logger = ctx.Logger(rli)
	return nil
}

// ServeHTTP 实现caddyhttp.MiddlewareHandler接口
// 这个方法在处理内部重定向时应用限速
func (rli *RateLimitInterceptor) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// 记录请求信息
	rli.logger.Debug("拦截器处理请求", 
		zap.String("method", r.Method), 
		zap.String("path", r.URL.Path),
		zap.String("remoteAddr", r.RemoteAddr))

	// 从请求上下文中获取令牌桶
	bucket := GetTokenBucketFromContext(r)
	if bucket == nil {
		// 如果没有令牌桶，直接放行
		rli.logger.Debug("未找到令牌桶，跳过限速")
		return next.ServeHTTP(w, r)
	}

	// 记录令牌桶信息
	rli.logger.Debug("找到令牌桶，应用限速", 
		zap.Int64("rate", bucket.Rate()),
		zap.Float64("tokens", bucket.Tokens()))

	// 创建限速响应写入器
	rateLimitWriter := NewRateLimitWriter(w, bucket, rli.logger)
	
	// 使用限速写入器处理响应
	rli.logger.Debug("应用限速写入器")
	return next.ServeHTTP(rateLimitWriter, r)
}

// parseInterceptorCaddyfile 解析 rate_limit_interceptor 指令
func parseInterceptorCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var rli RateLimitInterceptor

	// 解析块内容
	for h.Next() {
		// 指令行不应有参数
		if h.NextArg() {
			return nil, h.ArgErr()
		}

		// 不需要额外配置
		break
	}

	return &rli, nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*RateLimitInterceptor)(nil)
	_ caddyhttp.MiddlewareHandler = (*RateLimitInterceptor)(nil)
)
