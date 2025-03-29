# Caddy 带宽限速模块需求规格

> 版本: 1.4 (修正实现细节理解)
> 最后更新: 2025年3月29日

## 1. 核心功能: 基于 X-Accel-Redirect 的动态限速

模块的核心目标是根据后端应用通过 `X-Accel-*` 响应头传递的指令，对 Caddy 内部重定向（由 `X-Accel-Redirect` 触发）的内容传输进行动态限速。

**工作流程:**

1.  **客户端请求:** 客户端向 Caddy 发起请求（例如 `/download/file.zip`）。
2.  **Caddy 路由:** 请求匹配包含 `rate_limit_dynamic`, `rate_limit_interceptor`, `reverse_proxy`, `file_server` 等指令的路由。
3.  **`rate_limit_dynamic` (首次执行):**
    *   模块被调用，记录请求信息。
    *   创建 `captureResponseWriter` 包装原始 `ResponseWriter`。
    *   调用 `next.ServeHTTP` 将请求传递给后续处理器（`rate_limit_interceptor` -> `reverse_proxy`）。
4.  **`rate_limit_interceptor` (首次执行):**
    *   模块被调用。
    *   检查请求上下文，未找到令牌桶。
    *   调用 `next.ServeHTTP` 将请求传递给 `reverse_proxy`。
5.  **`reverse_proxy`:**
    *   将请求转发给后端应用。
    *   接收后端的响应（状态码 200，空响应体，包含 `X-Accel-*` 头）。
6.  **`rate_limit_dynamic` (处理响应):**
    *   控制权返回到 `rate_limit_dynamic` 的 `ServeHTTP` 方法中 `next.ServeHTTP` 之后的部分。
    *   模块从 `captureResponseWriter` 中检测到 `X-Accel-Redirect` 响应头。
    *   读取 `X-Accel-User-ID` 和 `X-Accel-RateLimit` 响应头。
    *   调用 `getOrCreateBucket` 获取或创建 `TokenBucket` 实例。
    *   使用 `context.WithValue` 将 `TokenBucket` 存入新的请求上下文 `ctx`。
    *   **执行内部重定向:**
        *   克隆原始请求 `r` 并附加新的上下文 `ctx`，得到 `newReq`。
        *   修改 `newReq` 的 URI 为 `X-Accel-Redirect` 指定的路径（例如 `/internal/file.zip`）。
        *   再次调用 `rl.next.ServeHTTP(w, newReq)`，将**修改后的请求**和**原始 `ResponseWriter` (`w`)** 传递给**同一个 `next` 处理器**（即 `rate_limit_interceptor`）。
        *   返回 `nil`，因为响应将由内部重定向处理。
7.  **`rate_limit_interceptor` (第二次执行):**
    *   模块再次被调用，但这次处理的是内部重定向请求（URI 为 `/internal/file.zip`），并且请求上下文中包含 `TokenBucket`。
    *   调用 `GetTokenBucketFromContext` 成功获取到 `TokenBucket`。
    *   创建 `RateLimitWriter` 实例，包装原始的 `ResponseWriter` (`w`)。
    *   调用 `next.ServeHTTP`，将**原始请求对象 `r`** 和**包装后的 `RateLimitWriter`** 传递给后续处理器。
8.  **`reverse_proxy` (第二次执行):**
    *   被调用，但请求 URI (`/internal/file.zip`) 不匹配其路径匹配器 (`/download/*`)，不执行操作，调用 `next`。
9.  **`file_server`:**
    *   被调用，处理请求 URI (`/internal/file.zip`)。
    *   查找文件（例如 `/srv/protected_files/file.zip`）。
    *   将文件内容写入传递过来的 `ResponseWriter`（实际上是 `RateLimitWriter`）。
10. **`RateLimitWriter`:**
    *   应用令牌桶算法，阻塞写入操作，将传输速率限制在指定值。
11. **客户端接收限速后的文件内容。**

**核心要求:**

*   **触发条件:** 模块的核心逻辑仅在后端响应包含 `X-Accel-Redirect` 头时触发。
*   **信号来源:** 限速指令 (`X-Accel-User-ID`, `X-Accel-RateLimit`) 来源于**后端响应头**。
*   **限速目标:** 限速应用于由 `X-Accel-Redirect` 触发的**内部重定向内容的传输**。
*   **用户识别:** 用户身份由后端在响应头中提供。
*   **速率提取:** 限速值由后端在响应头中提供。
*   **限速算法:** 采用令牌桶算法。
*   **限速行为:** 阻塞式传输，平滑流量。
*   **状态管理:** 需要为每个 `user_id` 维护独立的令牌桶状态。

## 1.3. 存储后端支持
模块必须支持以下两种存储后端来管理限速状态：

*   **内存模式 (默认):**
    *   限速状态存储在 Caddy 实例的内存中。
    *   适用于单实例部署或不需要跨实例同步状态的场景。
    *   配置简单，无需外部依赖。
*   **Redis 模式:**
    *   利用 Redis 作为共享存储后端，实现跨多个 Caddy 实例的分布式限速。
    *   **配置项:** 支持配置 Redis 服务器地址、密码和数据库编号。
    *   **状态同步:** 利用 Redis 存储和同步各用户的令牌桶状态。
    *   **原子操作:** 必须通过 Redis 原子操作（如 Lua 脚本或原生命令）实现令牌的补充与消耗逻辑，保证分布式环境下的数据一致性。

## 2. 开发与集成规范

### 2.1. 开发环境
*   **Docker 化开发 (必须):** 必须使用 Docker 容器作为开发环境，不在本地主机安装 Go 语言环境。Docker 可用于编译、测试和管理 Go 依赖（例如生成 `go.sum`）。

### 2.2. Caddy 2.x 兼容性
*   **模块ID:**
    *   `http.handlers.rate_limit_dynamic`
    *   `http.handlers.rate_limit_interceptor`
*   **架构:** 遵循 Caddy 2.x 插件开发规范，利用其模块化系统和配置 API。
*   **构建:** 使用 `xcaddy` 工具构建包含此模块的 Caddy 二进制文件。
*   **CI/CD:** 编译和发布工作应在 GitHub Actions 等 CI/CD 平台进行自动化。
*   **模块化:** 将模块开发为独立的 Go 模块，使用 Go Modules 管理依赖。
*   **接口实现:** 实现 Caddy 定义的标准接口 (`caddy.Module`, `caddy.Provisioner`, `caddy.Validator`, `caddyhttp.MiddlewareHandler` 等)。
*   **配置格式:** 同时支持 Caddyfile 和 JSON 两种配置格式。
*   **Caddyfile 指令:**
    *   `rate_limit_dynamic`: 拦截响应，准备限速器和上下文，并**执行内部重定向**。
    *   `rate_limit_interceptor`: 应用限速包装器。
*   **指令顺序:** 通过 `httpcaddyfile.RegisterDirectiveOrder` 确保 `rate_limit_dynamic` 在 `reverse_proxy` **之前**运行，`rate_limit_interceptor` 在 `file_server` **之前**运行。这种顺序依赖于 `rate_limit_dynamic` 内部调用 `next` 两次的机制来实现响应处理和内部重定向。

## 3. 资源管理与优化

### 3.1. 资源生命周期管理
*   **限速器过期:** 实现机制自动清理长期不活跃用户的限速状态。
    *   **内存模式:** 使用内置定时器 (`time.Ticker`, 5分钟间隔) 定期扫描并清理超过 30 分钟未访问的内存中的限速器引用。
    *   **Redis 模式:** 利用 Redis 的 Key TTL (Time To Live) 机制自动过期（默认 30 分钟）。

### 3.2. 性能优化 (Redis 模式)
*   **Redis 连接管理:** 使用 `go-redis/v9` 客户端库，该库内置连接池管理。

### 3.3. 高可用性 (Redis 模式)
*   **健康检查:** 实现对 Redis 连接的启动时 PING 检查和后台定时 PING 检查（30秒间隔）。
*   **服务降级:** 当 Redis 连接不可用时，自动降级为“放行”策略（返回最大令牌数），保证核心服务不受影响。

## 4. 技术实现细节

### 4.1 模块架构与执行流程

采用双中间件架构，利用 Caddy 中间件调用链和 `rate_limit_dynamic` 内部触发的重定向机制实现：

1.  **`rate_limit_dynamic` (`http.handlers.rate_limit_dynamic`)**:
    *   作为 HTTP 处理器，在 Caddyfile 中应放置在**可能**处理请求的处理器（如 `reverse_proxy`, `file_server`）**之前**（根据注册的 `Before` 顺序）。
    *   **首次执行 (处理请求):**
        *   创建 `captureResponseWriter` 包装 `ResponseWriter`。
        *   调用 `next.ServeHTTP` 将请求传递给后续处理器链。
    *   **处理响应 (首次执行返回后):**
        *   检查 `captureResponseWriter` 中捕获的响应头是否存在 `X-Accel-Redirect`。
        *   如果存在，读取 `X-Accel-User-ID` 和 `X-Accel-RateLimit`。
        *   调用 `getOrCreateBucket` 获取或创建 `TokenBucket` 实例。
        *   使用 `context.WithValue` 将 `TokenBucket` 存入新的请求上下文 `ctx`。
        *   **执行内部重定向:** 克隆原始请求 `r` 并附加新上下文 `ctx` 得到 `newReq`，修改 `newReq` 的 URI 为 `X-Accel-Redirect` 的值，然后再次调用 `rl.next.ServeHTTP(w, newReq)`，将控制权**重新**传递给后续处理器链（从 `rate_limit_interceptor` 开始）。
        *   返回 `nil`。
    *   **如果不存在 `X-Accel-Redirect`:** 将捕获到的原始响应写回客户端。

2.  **`rate_limit_interceptor` (`http.handlers.rate_limit_interceptor`)**:
    *   作为 HTTP 处理器，在 Caddyfile 中应放置在 `rate_limit_dynamic` **之后**，但在最终内容处理器（如 `file_server`）**之前**。
    *   **首次执行 (处理原始请求):**
        *   检查上下文，未找到 `TokenBucket`。
        *   调用 `next.ServeHTTP` 将请求传递给后续处理器（如 `reverse_proxy`）。
    *   **第二次执行 (处理内部重定向请求):**
        *   检查上下文，成功获取到 `TokenBucket`。
        *   创建 `RateLimitWriter` 实例包装原始的 `ResponseWriter` (`w`)。
        *   调用 `next.ServeHTTP`，将**包装后的 `ResponseWriter`** 传递给后续处理器（如 `file_server`）。

### 4.2 关键组件

#### 4.2.1 令牌桶 (`token_bucket.go`)

*   **结构体:**
    ```go
    type TokenBucket struct {
        rate           int64         // 令牌生成速率（字节/秒）
        tokens         float64       // 当前可用令牌数
        lastAccess     time.Time     // 最后访问时间
        mutex          sync.RWMutex  // 读写互斥锁
        storage        Storage       // 存储后端
        userID         string        // 用户ID
        logger         *zap.Logger   // 日志记录器
        burstMultiplier float64      // 突发倍数
    }
    ```
*   **初始化:** `NewTokenBucket` 函数接收 `rate`, `storage`, `userID`, `logger`, `burstMultiplier` 参数。初始令牌数 (`tokens`) 设置为 0，以防止初始突发。
*   **`Allow` 方法:**
    1.  计算自上次访问以来的时间差 `elapsed`。
    2.  增加令牌：`tokens += float64(rate) * elapsed`。
    3.  应用突发上限：`maxTokens := float64(rate) * burstMultiplier`，`tokens = min(tokens, maxTokens)`。
    4.  检查令牌是否足够：`tokens >= float64(count)`。
    5.  如果足够，则消耗令牌：`tokens -= float64(count)`，更新 `storage`，返回 `true`。
    6.  如果不足，返回 `false`。
*   **并发安全:** 使用 `sync.RWMutex` 保护内部状态。

#### 4.2.2 限速写入器 (`rate_limiter_writer.go`)

*   **结构体:**
    ```go
    type RateLimitWriter struct {
        w           http.ResponseWriter
        bucket      *TokenBucket
        logger      *zap.Logger
        wroteHeader bool
    }
    ```
*   **`Write` 方法:**
    1.  将写入数据 `b` 分块处理，默认块大小 `chunkSize` 为 64KB。
    2.  对每个块循环执行：
        *   调用 `bucket.Allow(chunkSize)` 尝试获取令牌。
        *   如果 `Allow` 返回 `false`（令牌不足）：
            *   计算所需等待时间：`waitTime = (chunkSize - bucket.Tokens()) / rate`。
            *   确保 `waitTime` 至少为 1 毫秒。
            *   调用 `time.Sleep(waitTime)` 阻塞。
        *   当 `Allow` 返回 `true` 后，调用底层 `w.Write()` 写入数据块。
*   **接口传递:** 实现了 `Header()`, `WriteHeader()`, 并尝试传递 `Flush()`, `Hijack()`, `Push()`。

#### 4.2.3 存储 (`storage.go`)

*   定义了 `Storage` 接口 (`Get`, `Set`, `Delete`, `Close`)。
*   **`MemoryStorage`:** 使用 `map[string]*bucketState` 和 `sync.RWMutex` 实现内存存储。状态清理依赖 `ratelimit.go` 中的定时器。
*   **`RedisStorage`:**
    *   使用 `go-redis/v9` 客户端。
    *   通过 Lua 脚本执行原子化的 `Get` 和 `Set` 操作。
    *   `Set` 操作同时使用 `EXPIRE` 设置 TTL (30分钟)。
    *   包含健康检查和连接失败时的降级逻辑。
    *   `Get` 操作使用 `strconv` 处理从 Lua 返回的 `interface{}`。

### 4.3 上下文传递

*   使用 `context.WithValue` 和自定义的 `contextKey` (`tokenBucketKey`) 在 `rate_limit_dynamic` 中存储 `TokenBucket`。
*   提供 `GetTokenBucketFromContext(r *http.Request)` 辅助函数供 `rate_limit_interceptor` 获取 `TokenBucket`。

### 4.4 Caddyfile 解析 (`caddyfile.go`)

*   `rate_limit_dynamic` 指令解析器 (`parseCaddyfile`) 支持 `header_user_id`, `header_rate_limit`, `burst_multiplier`, `redis` 子指令。
*   `rate_limit_interceptor` 指令解析器 (`parseInterceptorCaddyfile`) 不接受子指令。
*   **待办:** `UnmarshalCaddyfile` 函数（用于 JSON->Caddyfile 转换等）目前**缺少**对 `burst_multiplier` 的解析，需要补充。

### 4.5 日志

*   在关键路径（令牌操作、限速等待、错误处理等）添加了 `Info` 和 `Debug` 级别的日志，使用 `go.uber.org/zap`。

## 5. 非功能性需求

### 5.1. 健壮性
*   **错误处理:** 实现全面的错误处理，覆盖配置解析、用户识别、Redis 通信等所有关键环节。Redis 操作包含简单的重试逻辑。

### 5.2. 可观测性
*   **日志记录:** 集成 Caddy 日志系统，记录详细的操作日志和错误信息，可通过 Caddyfile 配置日志级别。

### 5.3. 并发安全
*   **线程安全:** 使用 `sync.RWMutex` 保护内存存储和令牌桶内部状态，确保多请求并发访问的安全性。

### 5.4. 测试覆盖
*   **单元测试 & 集成测试:** 需要编写或更新测试用例，以覆盖基于 `X-Accel-Redirect` 的核心流程、边界条件和并发场景。
*   **测试范围:** 现阶段测试重点为内存模式下的功能正确性，Redis 模式的功能实现即可，无需进行专项测试。

### 5.5. 协议兼容
*   **HTTP/1.1 & HTTP/2:** 确保限速逻辑在 HTTP/1.1 和 HTTP/2 协议下均能有效工作（基于 Caddy 的底层支持）。

## 6. 文档化

### 6.1. 用户文档
*   提供清晰、完整的模块说明文档，解释模块功能、配置选项和工作原理。

### 6.2. 配置示例 (最终测试成功配置)
*   **推荐配置:**
    ```caddyfile
    {
        debug
        log {
            level DEBUG
            format console
            output stdout
        }
    }

    :8081 {
        log {
            level DEBUG
            format console
            output stdout
        }

        # 测试路由
        route /hello {
            respond "Hello, World!" 200
        }

        # 注册动态限速中间件 (在 reverse_proxy 之前运行)
        # 它会拦截响应并可能触发内部重定向
        rate_limit_dynamic {
            header_user_id X-Accel-User-ID
            header_rate_limit X-Accel-RateLimit
            burst_multiplier 2.0 # 可选，默认 1.0
            # redis 127.0.0.1:6379 # 可选
        }

        # 注册拦截器中间件 (在 file_server 之前运行)
        # 它会检查上下文并应用限速包装器
        rate_limit_interceptor

        # 反向代理到后端服务器 (匹配 /download/*)
        reverse_proxy /download/* localhost:8080 {
             header_up X-Accel-Buffering no
        }

        # 处理内部重定向和未匹配的请求
        # (无路径匹配器，会处理 /internal/* 和其他所有请求)
        file_server {
            root /srv/protected_files
        }

        # 默认响应 (如果 file_server 未处理)
        # respond "Not Found" 404
    }
    ```
*   **说明:** 这个配置依赖于 Caddy 的指令排序 (`rate_limit_dynamic` before `reverse_proxy`, `rate_limit_interceptor` before `file_server`) 以及 `rate_limit_dynamic` 内部执行重定向的机制。`file_server` 没有路径匹配器，会处理所有未被 `reverse_proxy` 处理的请求，包括内部重定向后的请求。

## 7. 结论

通过深入理解 Caddy 的中间件机制和 HTTP 请求处理流程，并采用双模块协作以及内部重定向的实现方式，成功实现了一个功能完善的动态限速模块。该模块能够基于后端应用提供的 X-Accel 头部信息，对内部重定向的内容传输进行精确限速，同时支持突发流量控制和分布式部署。

这种实现方案不仅满足了当前的需求，还为未来的功能扩展提供了良好的基础。通过模块化设计和清晰的接口定义，我们可以轻松添加新功能，如更复杂的限速策略、更多的存储后端支持等。
