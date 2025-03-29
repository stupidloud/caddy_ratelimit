# 使用官方 Go 镜像作为基础镜像
FROM golang:1.21-alpine AS builder

# 设置工作目录
WORKDIR /app

# 复制 Go 模块文件
COPY go.mod go.sum ./

# 下载依赖项 (利用 Docker 缓存)
RUN go mod download

# 复制项目源代码
COPY . .

# 运行测试
CMD ["go", "test", "./..."]
