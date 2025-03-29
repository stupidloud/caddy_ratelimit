# Caddy 动态带宽限速模块

> 版本: 1.1
> 最后更新: 2025/3/29

这是一个为 Caddy 2.x 开发的动态带宽限速模块，基于 X-Accel-Redirect 响应头实现动态限速，无需静态配置限速参数。

## 核心功能

### 基于 X-Accel-Redirect 的动态限速

模块基于 X-Accel-Redirect 机制实现动态限速，工作流程如下：

1. 客户端请求 Caddy
2. Caddy 将请求转发给后端应用
3. 后端应用验证后返回一个**响应**，其中包含：
   - `X-Accel-Redirect: <internal_uri>` (必需)
   - `X-Accel-User-ID: <user_id>` (必需)
   - `X-Accel-RateLimit: <rate_in_bytes_per_sec>` (必需)
4. 模块拦截这个响应，读取头信息，创建限速器，并将其与当前请求关联
5. 当 Caddy 处理内部重定向（如 file_server）时，使用关联的限速器限制响应体传输速率

核心特性：

- **用户识别**: 从 `X-Accel-User-ID` 响应头中提取用户唯一标识
- **速率提取**: 从 `X-Accel-RateLimit` 响应头中提取限速值，单位为字节/秒（如 "1048576" 表示 1MB/s）
- **动态管理**: 为每个动态提取的用户标识创建独立的限速器，参数完全由响应头决定
- **平滑限速**: 当请求所需带宽超过当前可用令牌时，模块将**阻塞**响应传输，直到有足够令牌可用，而不是拒绝请求，这样可以平滑流量，确保传输速率不超过限制

### 存储后端支持

模块支持两种存储后端来管理限速状态：

- **内存模式 (默认)**: 限速状态存储在 Caddy 实例的内存中，适用于单实例部署
- **Redis 模式**: 利用 Redis 作为共享存储后端，实现跨多个 Caddy 实例的分布式限速

## 安装

### 使用 xcaddy 构建

```bash
xcaddy build --with github.com/example/caddy_rate_limit_module
```

### 从预构建版本安装

从 [GitHub Releases](https://github.com/example/caddy_rate_limit_module/releases) 下载预构建的二进制文件。

## 配置示例

### 基本配置

```
example.com {
    # 第一步：配置限速模块，处理响应头
    rate_limit_dynamic {
        header_user_id X-Accel-User-ID
        header_rate_limit X-Accel-RateLimit
    }
    
    # 第二步：配置拦截器，用于内部重定向时应用限速
    route {
        reverse_proxy backend:8080
        
        # 使用 Caddy 的 intercept 指令处理 X-Accel-Redirect
        intercept {
            @accel header X-Accel-Redirect *
            handle_response @accel {
                # 应用限速拦截器
                rate_limit_interceptor
                
                # 重写路径为 X-Accel-Redirect 头的值
                rewrite * {resp.header.X-Accel-Redirect}
                
                # 使用 GET 方法
                method GET
                
                # 提供文件
                file_server
            }
        }
    }
}
```

### 使用 Redis 存储

```
example.com {
    # 使用 Redis 存储
    rate_limit_dynamic {
        redis redis://:password@127.0.0.1:6379/0
        header_user_id X-Accel-User-ID
        header_rate_limit X-Accel-RateLimit
    }
    
    route {
        reverse_proxy backend:8080
        
        intercept {
            @accel header X-Accel-Redirect *
            handle_response @accel {
                rate_limit_interceptor
                rewrite * {resp.header.X-Accel-Redirect}
                method GET
                file_server
            }
        }
    }
}
```

## 高级特性

### 资源生命周期管理

- **限速器过期**: 自动清理长期不活跃用户的限速状态
  - 内存模式: 使用内置定时器定期扫描并清理过期条目
  - Redis 模式: 利用 Redis 的 Key TTL 机制自动过期

### 高可用性 (Redis 模式)

- **健康检查**: 实现对 Redis 连接的健康检查
- **服务降级**: 当 Redis 连接不可用时，自动降级为"放行"策略，保证核心服务不受影响

## 开发与贡献

### 环境要求

- Docker (用于开发环境)

### 测试

```bash
docker run \
  -v "$(pwd)":/app \
  -v go-mod-cache:/go/pkg/mod \
  -v go-build-cache:/root/.cache/go-build \
  -w /app \
  golang:1.21-alpine go test ./...
```

## 许可证

MIT
