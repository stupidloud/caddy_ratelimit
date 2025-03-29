package ratelimit

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// RateLimitWriter 实现一个限速的http.ResponseWriter
type RateLimitWriter struct {
	w           http.ResponseWriter
	bucket      *TokenBucket
	logger      *zap.Logger
	wroteHeader bool
}

// NewRateLimitWriter 创建一个新的限速响应写入器
func NewRateLimitWriter(w http.ResponseWriter, bucket *TokenBucket, logger *zap.Logger) *RateLimitWriter {
	return &RateLimitWriter{
		w:      w,
		bucket: bucket,
		logger: logger,
	}
}

// Header 实现http.ResponseWriter接口
func (rlw *RateLimitWriter) Header() http.Header {
	return rlw.w.Header()
}

// WriteHeader 实现http.ResponseWriter接口
func (rlw *RateLimitWriter) WriteHeader(statusCode int) {
	if !rlw.wroteHeader {
		rlw.wroteHeader = true
		rlw.w.WriteHeader(statusCode)
	}
}

// Write 实现http.ResponseWriter接口，添加限速逻辑
func (rlw *RateLimitWriter) Write(b []byte) (int, error) {
	if !rlw.wroteHeader {
		rlw.WriteHeader(http.StatusOK)
	}

	// 如果数据量为0，直接返回
	if len(b) == 0 {
		return 0, nil
	}

	// 根据速率动态调整块大小，提高高速率下的性能
	var chunkSize int
	rate := rlw.bucket.Rate()
	
	// 对于高速率（10-100MB/s），使用更大的块大小
	if rate >= 10*1024*1024 && rate < 50*1024*1024 { // 10-50MB/s
		chunkSize = 256 * 1024 // 256KB
	} else if rate >= 50*1024*1024 { // 50MB/s以上
		chunkSize = 512 * 1024 // 512KB
	} else {
		chunkSize = 64 * 1024 // 默认64KB
	}
	
	var written int

	// 记录开始写入的日志
	rlw.logger.Debug("开始限速写入", 
		zap.Int("totalBytes", len(b)), 
		zap.Int("chunkSize", chunkSize),
		zap.Int64("rate", rate))

	for written < len(b) {
		// 计算当前块大小
		remainingBytes := len(b) - written
		currentChunkSize := chunkSize
		if remainingBytes < chunkSize {
			currentChunkSize = remainingBytes
		}

		// 等待获取足够的令牌
		startWait := time.Now()
		waitCount := 0
		for !rlw.bucket.Allow(int64(currentChunkSize)) {
			waitCount++
			// 如果没有足够的令牌，计算精确的等待时间
			currentRate := rlw.bucket.Rate()
			if currentRate <= 0 {
				currentRate = 1024 // 默认1KB/s
			}
			
			// 计算需要等待的时间：(需要的令牌数 - 当前令牌数) / 速率 = 需要的秒数
			waitTokens := float64(currentChunkSize) - rlw.bucket.Tokens()
			waitTime := time.Duration(waitTokens / float64(currentRate) * float64(time.Second))
			
			// 确保等待时间至少为1毫秒，避免CPU空转
			if waitTime < time.Millisecond {
				waitTime = time.Millisecond
			}
			
			// 对于高速率，减少日志频率
			if waitCount == 1 || waitCount%10 == 0 || waitTime > 100*time.Millisecond {
				rlw.logger.Debug("限速等待", 
					zap.Duration("waitTime", waitTime), 
					zap.Int("chunkSize", currentChunkSize), 
					zap.Int64("rate", currentRate),
					zap.Float64("currentTokens", rlw.bucket.Tokens()),
					zap.Float64("waitTokens", waitTokens),
					zap.Int("waitCount", waitCount))
			}
			
			time.Sleep(waitTime)
		}

		// 如果等待时间超过阈值，记录日志
		waitDuration := time.Since(startWait)
		if waitDuration > 100*time.Millisecond {
			rlw.logger.Debug("限速等待完成", 
				zap.Duration("totalWaitTime", waitDuration), 
				zap.Int("waitCount", waitCount))
		}

		// 写入当前块
		n, err := rlw.w.Write(b[written:written+currentChunkSize])
		written += n
		
		// 如果写入出错，记录日志并返回
		if err != nil {
			rlw.logger.Error("写入错误", zap.Error(err), zap.Int("writtenBytes", written))
			return written, err
		}
		
		// 对于高速率，只在特定情况下记录进度
		if rate < 10*1024*1024 || written == len(b) || written%(1024*1024) == 0 {
			rlw.logger.Debug("写入进度", 
				zap.Int("writtenBytes", written), 
				zap.Int("totalBytes", len(b)), 
				zap.Float64("progress", float64(written)/float64(len(b))*100))
		}
	}

	// 记录完成日志
	rlw.logger.Debug("限速写入完成", zap.Int("totalBytes", written))
	return written, nil
}

// Hijack 实现http.Hijacker接口（如果底层ResponseWriter支持）
func (rlw *RateLimitWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := rlw.w.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, fmt.Errorf("底层ResponseWriter不支持http.Hijacker接口")
}

// Flush 实现http.Flusher接口（如果底层ResponseWriter支持）
func (rlw *RateLimitWriter) Flush() {
	if flusher, ok := rlw.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Push 实现http.Pusher接口（如果底层ResponseWriter支持）
func (rlw *RateLimitWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := rlw.w.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return fmt.Errorf("底层ResponseWriter不支持http.Pusher接口")
}
