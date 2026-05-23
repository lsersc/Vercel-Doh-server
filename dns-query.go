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
	"net/http"
	"sync"
	"time"
)

// ── 配置 ──────────────────────────────────────────────────────────────────────

var upstreams = []string{
	"https://1.0.0.1/dns-query",
	"https://8.8.4.4/dns-query",
	"https://208.67.220.220/dns-query",
	"https://dns.nextdns.io/dns-query",
	"https://doh.opendns.com/dns-query",
	"https://unfiltered.adguard-dns.com/dns-query",
	"https://freedns.controld.com/p0",
	"https://public.dns.iij.jp/dns-query",
	"https://doh.dns.sb/dns-query",
	"https://jp.tiar.app/dns-query",
	"https://dns.dnsguard.pub/dns-query",
	"https://doh.cleanbrowsing.org/doh/security-filter/",
	"https://wikimedia-dns.org/dns-query",
	"https://doh.ffmuc.net/dns-query",
	"https://sky.rethinkdns.com/dns-query",
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

// ── HTTP 客户端（4 秒超时，对应 PHP CURLOPT_TIMEOUT）───────────────────────

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

// queryUpstream 向单个上游发送 DoH 请求。
// 返回 (响应体, rcode==0)；任何错误返回 (nil, false)。
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

	// DNS wire format：第 4 字节低 4 位为 RCODE
	rcode := 2
	if len(data) >= 4 {
		rcode = int(data[3]) & 0x0f
	}
	return data, rcode == 0
}

// ── 批量并发查询 ──────────────────────────────────────────────────────────────

type batchResult struct {
	data []byte
	ok   bool // rcode == 0
}

// queryBatch 并发查询一批上游。
//   - 有任意一个 rcode==0 → 立即取消其余请求，返回 (data, true)
//   - 无 rcode==0 但有响应   → 返回 (first_failure, false)
//   - 全部超时/失败          → 返回 (nil, false)
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

	// 所有 goroutine 完成后关闭 channel
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	var firstFailure []byte
	for res := range resultCh {
		if res.ok {
			cancel() // 取消其余正在进行的请求
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

	// 构造转发给上游的查询串（仅 GET 需要）
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

	// 缓存 key = md5(method:extraQuery:body)
	cacheKey := fmt.Sprintf("doh_%x",
		md5.Sum([]byte(method+":"+extraQuery+":"+string(body))))

	// 命中缓存
	if cached, ok := cacheGet(cacheKey); ok {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(cached)
		return
	}

	// 随机打乱上游列表
	shuffled := make([]string, len(upstreams))
	copy(shuffled, upstreams)
	rand.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	// 按批次顺序尝试，每批内部并发
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

// ── 入口 ──────────────────────────────────────────────────────────────────────

func main() {
	http.HandleFunc("/dns-query", dohHandler)
	addr := ":8053"
	log.Printf("DoH proxy listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
