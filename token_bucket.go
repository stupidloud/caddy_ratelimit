package ratelimit

import (
	"sync"
	"time"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// TokenBucket 实现令牌桶算法进行限速
type TokenBucket struct {
	rate           int64         // 令牌生成速率（字节/秒）
	tokens         float64       // 当前可用令牌数
	lastAccess     time.Time     // 最后访问时间
	mutex          sync.RWMutex  // 读写互斥锁
	storage        Storage       // 存储后端
	userID         string        // 用户ID
	logger         *zap.Logger   // 日志记录器
	burstMultiplier float64      // 突发倍数
	lastStorageUpdate time.Time  // 上次存储更新时间
}

// 存储更新阈值，避免频繁更新存储
const storageUpdateThreshold = 5 * time.Second

// NewTokenBucket 创建新的令牌桶
func NewTokenBucket(rate int64, storage Storage, userID string, logger *zap.Logger, burstMultiplier float64) *TokenBucket {
	bucket := &TokenBucket{
		rate:           rate,
		tokens:         0, // 初始令牌数为0，避免突发流量
		lastAccess:     time.Now(),
		storage:        storage,
		userID:         userID,
		logger:         logger,
		burstMultiplier: burstMultiplier,
		lastStorageUpdate: time.Now(),
	}

	// 从存储中恢复状态
	if storage != nil {
		if tokens, lastAccess, err := storage.Get(userID); err == nil {
			bucket.tokens = tokens
			bucket.lastAccess = lastAccess
		}
	}

	// 使用条件日志
	if logger.Core().Enabled(zapcore.DebugLevel) {
		logger.Debug("创建令牌桶", 
			zap.String(logKeyUserID, userID), 
			zap.Int64(logKeyRate, rate),
			zap.Float64(logKeyTokens, bucket.tokens),
			zap.Float64("burstMultiplier", burstMultiplier))
	}

	return bucket
}

// Allow 检查是否允许消耗指定数量的令牌
func (tb *TokenBucket) Allow(count int64) bool {
	tb.mutex.Lock()
	defer tb.mutex.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastAccess).Seconds()
	tb.lastAccess = now

	// 根据经过的时间，添加新的令牌
	newTokens := float64(tb.rate) * elapsed
	tb.tokens += newTokens

	// 令牌数量上限为速率的burstMultiplier倍
	maxTokens := float64(tb.rate) * tb.burstMultiplier
	if tb.tokens > maxTokens {
		tb.tokens = maxTokens
	}

	// 使用条件日志并减少日志频率
	shouldLog := tb.logger.Core().Enabled(zapcore.DebugLevel) && 
		(newTokens > float64(tb.rate)/5 || count > int64(tb.rate)/5 || tb.tokens < float64(count))

	if shouldLog {
		tb.logger.Debug("令牌状态", 
			zap.String(logKeyUserID, tb.userID), 
			zap.Float64(logKeyElapsed, elapsed),
			zap.Float64(logKeyNewTokens, newTokens),
			zap.Float64(logKeyTotalTokens, tb.tokens),
			zap.Int64(logKeyRequestedCount, count),
			zap.Float64(logKeyMaxTokens, maxTokens))
	}

	// 检查是否有足够的令牌
	if tb.tokens < float64(count) {
		// 令牌不足，拒绝请求
		if tb.logger.Core().Enabled(zapcore.DebugLevel) {
			tb.logger.Debug("令牌不足", 
				zap.String(logKeyUserID, tb.userID), 
				zap.Float64(logKeyTokens, tb.tokens), 
				zap.Int64(logKeyCount, count))
		}
		return false
	}

	// 消耗令牌
	tb.tokens -= float64(count)

	// 更新存储，但限制更新频率
	if tb.storage != nil && time.Since(tb.lastStorageUpdate) > storageUpdateThreshold {
		tb.storage.Set(tb.userID, tb.tokens, tb.lastAccess)
		tb.lastStorageUpdate = time.Now()
	}

	// 减少日志频率
	if shouldLog {
		tb.logger.Debug("消耗令牌", 
			zap.String(logKeyUserID, tb.userID), 
			zap.Int64(logKeyCount, count),
			zap.Float64(logKeyRemainingTokens, tb.tokens))
	}

	return true
}

// Rate 获取令牌桶的速率
func (tb *TokenBucket) Rate() int64 {
	tb.mutex.RLock()
	defer tb.mutex.RUnlock()
	return tb.rate
}

// SetRate 设置令牌桶的速率
func (tb *TokenBucket) SetRate(rate int64) {
	tb.mutex.Lock()
	defer tb.mutex.Unlock()
	tb.rate = rate
}

// LastAccess 获取最后访问时间
func (tb *TokenBucket) LastAccess() time.Time {
	tb.mutex.RLock()
	defer tb.mutex.RUnlock()
	return tb.lastAccess
}

// Tokens 获取当前可用令牌数
func (tb *TokenBucket) Tokens() float64 {
	tb.mutex.RLock()
	defer tb.mutex.RUnlock()
	return tb.tokens
}
