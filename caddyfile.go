package ratelimit

import (
	"fmt"
	"strconv"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// parseCaddyfile 解析Caddyfile配置
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var rl RateLimit
	err := rl.UnmarshalCaddyfile(h.Dispenser)
	return &rl, err
}

// UnmarshalCaddyfile 实现 caddyfile.Unmarshaler 接口
func (rl *RateLimit) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// 解析块内容
	for d.Next() { // 外层循环应该只迭代一次，处理指令本身
		// 指令行不应有参数
		if d.NextArg() {
			return d.ArgErr()
		}

		// 进入块解析
		for d.NextBlock(0) {
			switch d.Val() {
			case "header_user_id":
				if !d.NextArg() {
					return d.ArgErr()
				}
				rl.HeaderUserID = d.Val()
			case "header_rate_limit":
				if !d.NextArg() {
					return d.ArgErr()
				}
				rl.HeaderRateLimit = d.Val()
			case "burst_multiplier":
				if !d.NextArg() {
					return d.ArgErr()
				}
				multiplier, err := strconv.ParseFloat(d.Val(), 64)
				if err != nil {
					return fmt.Errorf("无效的突发倍数: %v", err)
				}
				if multiplier <= 0 {
					return fmt.Errorf("突发倍数必须大于0")
				}
				rl.BurstMultiplier = multiplier
			case "redis":
				if !d.NextArg() {
					return d.ArgErr()
				}
				rl.Redis = d.Val()
			default:
				return d.Errf("未知的子指令 '%s'", d.Val())
			}
		}
		// 解析完块后退出外层循环
		break
	}

	return nil
}
