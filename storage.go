package ratelimit

import (
	"context"
	"fmt"
	"math"
	"strconv" // 添加 strconv
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Storage 定义限速器状态存储接口
type Storage interface {
	// Get 获取用户的令牌数量和最后访问时间
	Get(userID string) (float64, time.Time, error)
	
	// Set 设置用户的令牌数量和最后访问时间
	Set(userID string, tokens float64, lastAccess time.Time) error
	
	// Delete 删除用户的限速状态
	Delete(userID string) error
	
	// Close 关闭存储连接并释放资源
	Close() error
}

// MemoryStorage 内存存储实现
type MemoryStorage struct {
	data   map[string]*bucketState
	mutex  sync.RWMutex
	logger *zap.Logger
}

type bucketState struct {
	Tokens     float64
	LastAccess time.Time
}

// NewMemoryStorage 创建新的内存存储
func NewMemoryStorage(logger *zap.Logger) (*MemoryStorage, error) {
	return &MemoryStorage{
		data:   make(map[string]*bucketState),
		logger: logger,
	}, nil
}

// Get 从内存获取用户的令牌数量和最后访问时间
func (ms *MemoryStorage) Get(userID string) (float64, time.Time, error) {
	ms.mutex.RLock()
	defer ms.mutex.RUnlock()
	
	if state, exists := ms.data[userID]; exists {
		return state.Tokens, state.LastAccess, nil
	}
	
	return 0, time.Time{}, fmt.Errorf("用户 %s 不存在", userID)
}

// Set 设置用户的令牌数量和最后访问时间到内存
func (ms *MemoryStorage) Set(userID string, tokens float64, lastAccess time.Time) error {
	ms.mutex.Lock()
	defer ms.mutex.Unlock()
	
	ms.data[userID] = &bucketState{
		Tokens:     tokens,
		LastAccess: lastAccess,
	}
	
	return nil
}

// Delete 从内存删除用户的限速状态
func (ms *MemoryStorage) Delete(userID string) error {
	ms.mutex.Lock()
	defer ms.mutex.Unlock()
	
	delete(ms.data, userID)
	return nil
}

// Close 关闭内存存储
func (ms *MemoryStorage) Close() error {
	ms.mutex.Lock()
	defer ms.mutex.Unlock()
	
	ms.data = make(map[string]*bucketState)
	return nil
}

// RedisStorage Redis存储实现
type RedisStorage struct {
	client        *redis.Client
	keyPrefix     string
	healthyFlag   bool
	logger        *zap.Logger
	healthTicker  *time.Ticker
	healthDone    chan struct{}
}

// 返回最大令牌数和当前时间，用于Redis不可用或出错时
func (rs *RedisStorage) fallbackValues() (float64, time.Time, error) {
	return math.MaxFloat64, time.Now(), nil
}

// NewRedisStorage 创建新的Redis存储
func NewRedisStorage(redisURL string, logger *zap.Logger) (*RedisStorage, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		// 尝试作为简单地址解析
		opts = &redis.Options{
			Addr: redisURL,
		}
	}
	
	client := redis.NewClient(opts)
	
	// 测试连接
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	initialHealthy := true
	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("Redis连接失败，将降级为放行模式", zap.Error(err))
		initialHealthy = false
	} else {
		logger.Info("Redis连接成功")
	}
	
	rs := &RedisStorage{
		client:      client,
		keyPrefix:   "ratelimit:",
		healthyFlag: initialHealthy,
		logger:      logger,
		healthDone:  make(chan struct{}),
	}
	
	// 启动健康检查
	rs.healthTicker = time.NewTicker(30 * time.Second)
	go rs.healthCheck()
	
	return rs, nil
}

// 健康检查
func (rs *RedisStorage) healthCheck() {
	for {
		select {
		case <-rs.healthTicker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := rs.client.Ping(ctx).Err()
			cancel()
			
			if err != nil && rs.healthyFlag {
				rs.healthyFlag = false
				rs.logger.Warn("Redis连接失败，降级为放行模式", zap.Error(err))
			} else if err == nil && !rs.healthyFlag {
				rs.healthyFlag = true
				rs.logger.Info("Redis连接恢复")
			}
		case <-rs.healthDone:
			return
		}
	}
}

// Close 关闭Redis连接和健康检查
func (rs *RedisStorage) Close() error {
	// 停止健康检查
	if rs.healthTicker != nil {
		rs.healthTicker.Stop()
	}
	
	if rs.healthDone != nil {
		close(rs.healthDone)
	}
	
	// 尝试清理所有相关键
	if rs.client != nil && rs.healthyFlag {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		
		// 尝试查找并删除所有前缀匹配的键
		script := `
		local keys = redis.call('KEYS', ARGV[1])
		if #keys > 0 then
			return redis.call('DEL', unpack(keys))
		end
		return 0
		`
		
		pattern := rs.keyPrefix + "*"
		_, err := rs.client.Eval(ctx, script, []string{}, pattern).Result()
		if err != nil {
			rs.logger.Warn("关闭时清理Redis键失败", zap.Error(err))
		} else {
			rs.logger.Info("成功清理Redis键")
		}
		
		// 关闭客户端连接
		return rs.client.Close()
	}
	
	return nil
}

// Get 从Redis获取用户的令牌数量和最后访问时间
func (rs *RedisStorage) Get(userID string) (float64, time.Time, error) {
	if !rs.healthyFlag {
		// Redis不可用时返回最大令牌数，确保请求被放行
		return rs.fallbackValues()
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	
	key := rs.keyPrefix + userID
	
	// 使用Lua脚本原子获取数据
	script := `
	local tokens = redis.call('HGET', KEYS[1], 'tokens')
	local lastAccess = redis.call('HGET', KEYS[1], 'lastAccess')
	if tokens and lastAccess then
		return {tokens, lastAccess}
	else
		return nil
	end
	`
	
	// 尝试最多3次
	var result interface{}
	var err error
	
	for i := 0; i < 3; i++ {
		result, err = rs.client.Eval(ctx, script, []string{key}).Result()
		if err == nil {
			break
		}
		
		if err == redis.Nil {
			return 0, time.Time{}, fmt.Errorf("用户 %s 不存在", userID)
		}
		
		rs.logger.Warn("Redis获取数据失败，尝试重试", zap.Int("重试次数", i+1), zap.Error(err))
		time.Sleep(50 * time.Millisecond)
	}
	
	if err != nil {
		rs.logger.Error("Redis获取数据失败，所有重试均失败", zap.Error(err))
		return rs.fallbackValues()
	}
	
	resultSlice, ok := result.([]interface{})
	if !ok || len(resultSlice) != 2 {
		rs.logger.Error("Redis返回数据格式错误", zap.Any("result", result))
		return rs.fallbackValues()
	}

	// 使用 strconv 进行转换
	tokensStr := fmt.Sprintf("%v", resultSlice[0])
	tokens, err := strconv.ParseFloat(tokensStr, 64)
	if err != nil {
		rs.logger.Error("解析tokens失败", zap.String("value", tokensStr), zap.Error(err))
		return rs.fallbackValues()
	}

	lastAccessStr := fmt.Sprintf("%v", resultSlice[1])
	lastAccessUnixNano, err := strconv.ParseInt(lastAccessStr, 10, 64)
	if err != nil {
		rs.logger.Error("解析lastAccess失败", zap.String("value", lastAccessStr), zap.Error(err))
		return rs.fallbackValues()
	}

	lastAccess := time.Unix(0, lastAccessUnixNano) // 假设存储的是纳秒

	return tokens, lastAccess, nil
}

// Set 设置用户的令牌数量和最后访问时间到Redis
func (rs *RedisStorage) Set(userID string, tokens float64, lastAccess time.Time) error {
	if !rs.healthyFlag {
		// Redis不可用时不进行存储
		return nil
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	
	key := rs.keyPrefix + userID
	
	// 使用Lua脚本原子设置数据并设置过期时间
	script := `
	redis.call('HSET', KEYS[1], 'tokens', ARGV[1], 'lastAccess', ARGV[2])
	redis.call('EXPIRE', KEYS[1], 1800)  -- 30分钟过期
	return 1
	`
	
	// 尝试最多3次
	var err error
	
	for i := 0; i < 3; i++ {
		_, err = rs.client.Eval(ctx, script, []string{key}, tokens, lastAccess.UnixNano()).Result()
		if err == nil {
			break
		}
		
		rs.logger.Warn("Redis设置数据失败，尝试重试", zap.Int("重试次数", i+1), zap.Error(err))
		time.Sleep(50 * time.Millisecond)
	}
	
	if err != nil {
		rs.logger.Error("Redis设置数据失败，所有重试均失败", zap.Error(err))
	}
	
	return err
}

// Delete 从Redis删除用户的限速状态
func (rs *RedisStorage) Delete(userID string) error {
	if !rs.healthyFlag {
		return nil
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	
	key := rs.keyPrefix + userID
	
	// 尝试最多3次
	var err error
	
	for i := 0; i < 3; i++ {
		err = rs.client.Del(ctx, key).Err()
		if err == nil {
			break
		}
		
		rs.logger.Warn("Redis删除数据失败，尝试重试", zap.Int("重试次数", i+1), zap.Error(err))
		time.Sleep(50 * time.Millisecond)
	}
	
	if err != nil {
		rs.logger.Error("Redis删除数据失败，所有重试均失败", zap.Error(err))
	}
	
	return err
}
