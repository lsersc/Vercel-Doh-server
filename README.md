# Vercel 部署指南

## 项目结构

```
your-project/
├── vercel.json              # Vercel 配置
├── go.mod                   # Go 模块定义
├── api/
│   └── dns-query.go        # Serverless handler
└── dns-query.go            # （可选）本地开发版本
```

## 部署步骤

### 1. 初始化项目

```bash
mkdir doh-proxy && cd doh-proxy
git init
```

### 2. 创建 go.mod

```bash
cat > go.mod << 'EOF'
module github.com/yourusername/doh-proxy

go 1.21
EOF
```

### 3. 创建 API 目录和文件

```bash
mkdir api
# 把 api-dns-query.go 复制到 api/dns-query.go
cp api-dns-query.go api/dns-query.go
```

### 4. 复制 vercel.json

已提供，放在根目录。

### 5. 推送到 GitHub

```bash
git add .
git commit -m "Initial commit"
git branch -M main
git remote add origin https://github.com/yourusername/doh-proxy.git
git push -u origin main
```

### 6. 在 Vercel 部署

**方式 A：使用 Vercel CLI**
```bash
npm i -g vercel
vercel
```

**方式 B：Vercel Dashboard**
- 登录 https://vercel.com
- 点 "New Project"
- 导入 GitHub repo
- 自动检测 Go，无需额外配置
- 点 Deploy

### 7. 访问

部署后 Vercel 会给你一个 URL，如 `https://your-project.vercel.app`

你的 DoH 端点就是：`https://your-project.vercel.app/dns-query`

## 使用示例

### 测试 GET 请求

```bash
# macOS/Linux - 获取 example.com 的 A 记录
curl -v "https://your-project.vercel.app/dns-query?dns=AAABAAABAAAAAAAA" \
  -H "Accept: application/dns-message"
```

### 使用 dnscrypt-proxy、Stubby 等工具

在配置中指向你的 Vercel 端点：
```
[0]
address = https://your-project.vercel.app/dns-query
```

## 注意事项

### 缓存限制

- 本版本使用进程内缓存（600s TTL）
- Vercel Serverless 函数是无状态的，每次冷启动缓存会丢失
- 如需持久缓存，考虑接入 Redis（需付费）：

```go
// 伪代码示例 - 集成 Redis
import "github.com/redis/go-redis/v9"

var rdb = redis.NewClient(&redis.Options{Addr: os.Getenv("REDIS_URL")})
```

### 执行时间限制

- Vercel 标准版：**10 秒** 超时
- Pro 版本：**60 秒** 超时
- 本代码总 HTTP 超时为 4 秒/上游 × 3 个上游并发 = 单批最多 ~4 秒，应该没问题

### 内存与成本

- 函数 size ~10MB
- 免费额度通常足够个人用途
- 按执行时间和调用次数计费

## 本地测试

如果想本地开发测试，使用之前的 `dns-query.go`（带 `main()`）：

```bash
go run dns-query.go
# 访问 http://localhost:8053/dns-query?dns=...
```

## 环境变量配置（可选）

如果后续需要增加配置（如上游白名单、缓存 TTL），可以在 Vercel Dashboard 的 Environment Variables 中设置，然后在 Go 中读取：

```go
cacheTTL := time.Duration(mustParseInt(os.Getenv("CACHE_TTL"))) * time.Second
```

---

遇到问题？
- 查看 Vercel Dashboard 的函数日志
- 用 `vercel logs` CLI 命令查看
