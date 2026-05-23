package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// ── 配置 ──────────────────────────────────────────────────────────────────────

var upstreams = []string{
	"https://security.cloudflare-dns.com/dns-query",
    "https://dns.nextdns.io/dns-query",
    "https://doh.opendns.com/dns-query",
    "https://dns.adguard-dns.com/dns-query",
    "https://freedns.controld.com/p2",
    "https://dns.google/dns-query",
    "https://ordns.he.net/dns-query",
    "https://dns.quad9.net/dns-query",
}

const (
	cacheTTL  = 600 * time.Second
	batchSize = 3
)

// ── 简易内存缓存（带 TTL）────────────────────────────────────────────────────

type cacheEntry struct {
	data   []byte
	expiry time.Time
}

var dnsCache sync.Map

func cacheGet(key string) ([]byte, bool) {
	v, ok := dnsCache.Load(key)
	if !ok {
		return nil, false
	}
	e := v.(cacheEntry)
	if time.Now().After(e.expiry) {
		dnsCache.Delete(key)
		return nil, false
	}
	return e.data, true
}

func cacheSet(key string, data []byte) {
	dnsCache.Store(key, cacheEntry{
		data:   data,
		expiry: time.Now().Add(cacheTTL),
	})
}

// ── HTTP 客户端（4 秒超时）──────────────────────────────────────────────────

var dohClient = &http.Client{Timeout: 4 * time.Second}

// ── 工具函数 ─────────────────────────────────────────────────────────────────

func nowMs() int64 { return time.Now().UnixMilli() }

func writeErrorJSON(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"timestamp": nowMs(),
			"code":      code,
			"message":   message,
		},
	})
}

// ── 单个上游查询 ──────────────────────────────────────────────────────────────

func queryUpstream(ctx context.Context, upstream, method, extraQuery string, body []byte) ([]byte, bool) {
	url := upstream + extraQuery

	var req *http.Request
	var err error
	if method == http.MethodPost {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	}
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	resp, err := dohClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}

	rcode := 2
	if len(data) >= 4 {
		rcode = int(data[3]) & 0x0f
	}
	return data, rcode == 0
}

// ── 批量并发查询 ──────────────────────────────────────────────────────────────

type batchResult struct {
	data []byte
	ok   bool
}

func queryBatch(batch []string, method, extraQuery string, body []byte) ([]byte, bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resultCh := make(chan batchResult, len(batch))
	var wg sync.WaitGroup

	for _, up := range batch {
		wg.Add(1)
		go func(upstream string) {
			defer wg.Done()
			data, ok := queryUpstream(ctx, upstream, method, extraQuery, body)
			if data != nil {
				resultCh <- batchResult{data: data, ok: ok}
			}
		}(up)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	var firstFailure []byte
	for res := range resultCh {
		if res.ok {
			cancel()
			return res.data, true
		}
		if firstFailure == nil {
			firstFailure = res.data
		}
	}
	return firstFailure, false
}

// ── HTTP 处理器 ───────────────────────────────────────────────────────────────

func dohHandler(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	if method != http.MethodGet && method != http.MethodPost {
		writeErrorJSON(w, http.StatusMethodNotAllowed, "Method Not Allowed: only GET/POST supported")
		return
	}

	extraQuery := ""
	if method == http.MethodGet {
		if r.URL.RawQuery == "" {
			writeErrorJSON(w, http.StatusBadRequest, "Bad Request: GET must include query parameters")
			return
		}
		extraQuery = "?" + r.URL.RawQuery
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "Bad Request: failed to read body")
		return
	}

	cacheKey := fmt.Sprintf("doh_%x",
		md5.Sum([]byte(method+":"+extraQuery+":"+string(body))))

	if cached, ok := cacheGet(cacheKey); ok {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(cached)
		return
	}

	shuffled := make([]string, len(upstreams))
	copy(shuffled, upstreams)
	rand.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	for i := 0; i < len(shuffled); i += batchSize {
		end := i + batchSize
		if end > len(shuffled) {
			end = len(shuffled)
		}
		data, _ := queryBatch(shuffled[i:end], method, extraQuery, body)
		if data != nil {
			cacheSet(cacheKey, data)
			w.Header().Set("Content-Type", "application/dns-message")
			_, _ = w.Write(data)
			return
		}
	}

	writeErrorJSON(w, http.StatusBadGateway, "All upstream DoH failed")
}

// ── 主入口 ────────────────────────────────────────────────────────────────────

func main() {
	// 获取 Vercel 分配的端口，或本地默认 8080
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// 验证端口号
	if _, err := strconv.Atoi(port); err != nil {
		log.Fatalf("Invalid PORT: %s", port)
	}

	http.HandleFunc("/query-dns", dohHandler)

	// 健康检查端点（可选，Vercel 可能会用到）
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("DoH proxy is running"))
	})

	addr := ":" + port
	log.Printf("DoH proxy listening on %s", addr)

	// 使用 Vercel 推荐的方式：监听所有网卡
	listener, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	defer listener.Close()

	if err := http.Serve(listener, nil); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
