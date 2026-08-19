package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	clickFlushInterval = 30 * time.Second
	clickDedupWindow   = 10 * time.Minute
	maxClickBodyBytes  = 1 << 10
	maxDedupEntries    = 200000
	clickRatePerSecond = 20
	clickRateBurst     = 100
)

// clickFile 是落盘格式。
//
// 文件名绝不能带 "drivers-" 前缀：cmd/updater/main.go 的 removeStaleChunks
// 会 Glob("drivers-*.json") 并删掉所有不在 index.json 里的文件，
// 一旦命中，每周日 03:00 更新器跑完计数就归零。
//
// total 刻意不落盘，启动时遍历求和重建，避免与 counts 不一致。
type clickFile struct {
	Counts map[int]int64 `json:"counts"`
}

type clickCounter struct {
	path string

	mu     sync.Mutex
	counts map[int]int64
	total  int64
	// seq/flushed 是单调序号，不用 dirty bool：
	// flush 取完快照后到写完之间的新增点击会被 bool 误判为已落盘，
	// 低流量时可能整夜不再写盘。
	seq     int64
	flushed int64
	writeOK bool

	done chan struct{}
	wg   sync.WaitGroup
}

func newClickCounter(path string) *clickCounter {
	c := &clickCounter{
		path:   path,
		counts: make(map[int]int64),
		done:   make(chan struct{}),
	}
	c.load()

	// 启动即强制写一次，兼作写权限探针。
	// docker-compose 的 bind mount 会完全遮蔽镜像里的 --chown 设置，
	// 运行时到底能不能写，只有真写一次才知道。
	if err := c.writeNow(); err != nil {
		log.Printf("ERROR 点击计数无法写入 %s: %v（计数将只存在于内存，重启即丢）", path, err)
	} else {
		log.Printf("点击计数已加载: %d 条驱动，累计 %d 次", len(c.counts), c.total)
	}

	c.wg.Add(1)
	go c.loop()
	return c
}

func (c *clickCounter) load() {
	data, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("ERROR 读取点击计数失败: %v，从零开始", err)
		return
	}

	var file clickFile
	if err := json.Unmarshal(data, &file); err != nil {
		// 隔离而不是就地覆盖，坏文件留着可人工抢救
		quarantine := fmt.Sprintf("%s.corrupt.%d", c.path, time.Now().Unix())
		if renameErr := os.Rename(c.path, quarantine); renameErr != nil {
			log.Printf("ERROR 点击计数文件损坏且隔离失败: %v / %v", err, renameErr)
		} else {
			log.Printf("ERROR 点击计数文件损坏，已隔离为 %s，从零开始", quarantine)
		}
		return
	}

	for id, n := range file.Counts {
		if n <= 0 {
			continue
		}
		c.counts[id] = n
		c.total += n
	}
}

// add 记一次点击。调用方必须先用 driverStore.hasID 校验过 id。
func (c *clickCounter) add(id int) (int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[id]++
	c.total++
	c.seq++
	return c.counts[id], c.total
}

// lookup 读取单条计数，不产生任何写入。
func (c *clickCounter) lookup(id int) (int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[id], c.total
}

// pageCounts 取当页记录的计数。
// 读缺失键在 Go 里不会建桶，这是 counts 不会被读路径撑大的构造性保证。
func (c *clickCounter) pageCounts(items []driver) (map[int]int64, int64) {
	result := make(map[int]int64)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, item := range items {
		if n := c.counts[item.ID]; n > 0 {
			result[item.ID] = n
		}
	}
	return result, c.total
}

func (c *clickCounter) flush() error {
	c.mu.Lock()
	clean := c.seq == c.flushed
	c.mu.Unlock()
	if clean {
		return nil
	}
	return c.writeNow()
}

func (c *clickCounter) writeNow() error {
	// 持锁 marshal 是刻意的：省掉一整套 map 深拷贝，
	// 代价是每 30 秒阻塞约 10ms，而真正慢的磁盘写在锁外。
	c.mu.Lock()
	seq := c.seq
	data, err := json.Marshal(clickFile{Counts: c.counts})
	c.mu.Unlock()
	if err != nil {
		return err
	}

	if err := writeFileAtomic(c.path, data); err != nil {
		c.mu.Lock()
		c.writeOK = false
		c.mu.Unlock()
		return err // 不推进 flushed，下个 tick 自动重试
	}

	c.mu.Lock()
	c.flushed = seq
	c.writeOK = true
	c.mu.Unlock()
	return nil
}

func (c *clickCounter) loop() {
	defer c.wg.Done()
	ticker := time.NewTicker(clickFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := c.flush(); err != nil {
				log.Printf("ERROR 点击计数落盘失败: %v", err)
			}
		case <-c.done:
			return
		}
	}
}

func (c *clickCounter) Close() error {
	close(c.done)
	c.wg.Wait()
	return c.flush()
}

func (c *clickCounter) healthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeOK
}

// writeFileAtomic 先写临时文件再 rename，避免进程中途退出留下半截 JSON。
func writeFileAtomic(path string, data []byte) error {
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// clickDedup 记录 (客户端IP, 驱动ID) 最近一次计数时间，窗口内重复不计。
// 表本身设了上限：IPv6 键空间无限，不封顶就是新的内存增长面。
type clickDedup struct {
	mu     sync.Mutex
	window time.Duration
	seen   map[string]time.Time
}

func newClickDedup(window time.Duration) *clickDedup {
	return &clickDedup{window: window, seen: make(map[string]time.Time)}
}

// allow 判断这次点击是否应当计数。
//
// key 为空表示拿不到访客真实 IP，此时一律放行。这个 fail-open 是刻意的：
// 反代没透传真实 IP 时若按代理 IP 去重，所有访客会被当成同一个人，
// 99% 的点击被静默吞掉，而接口照返 200、页面照常跳转，极难发现。
func (d *clickDedup) allow(key string, now time.Time) bool {
	if key == "" {
		return true
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if last, ok := d.seen[key]; ok && now.Sub(last) < d.window {
		return false
	}

	if len(d.seen) >= maxDedupEntries {
		d.sweepLocked(now)
		if len(d.seen) >= maxDedupEntries {
			return true // 清不动就放行，绝不因为去重表满了而丢计数
		}
	}

	d.seen[key] = now
	return true
}

func (d *clickDedup) sweepLocked(now time.Time) {
	for key, seenAt := range d.seen {
		if now.Sub(seenAt) >= d.window {
			delete(d.seen, key)
		}
	}
}

// tokenBucket 是全局速率上限，兜住去重拦不住的批量刷。
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	rate   float64
	burst  float64
	last   time.Time
}

func newTokenBucket(rate, burst float64) *tokenBucket {
	return &tokenBucket{tokens: burst, rate: rate, burst: burst, last: time.Now()}
}

func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP 解析访客 IP。
//
// 直连时用 RemoteAddr；只有当 RemoteAddr 属于内网（说明请求来自反向代理）
// 才信任代理头，否则任何人加一个请求头就能绕过去重。
//
// 反代未透传真实 IP 时返回空串，让调用方放弃去重，
// 而不是退化成"按代理 IP 去重"——那等于把所有访客当成一个人。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote := net.ParseIP(strings.TrimSpace(host))
	if remote == nil {
		return ""
	}
	if !isTrustedProxy(remote) {
		return remote.String()
	}

	// X-Real-IP：nginx 用 $remote_addr 覆盖写入，客户端伪造的会被盖掉
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
		return ip.String()
	}

	// X-Forwarded-For 取最右侧一项：nginx 的 $proxy_add_x_forwarded_for
	// 是"客户端传来的值 + 追加 $remote_addr"，只有最右侧是代理写的、不可伪造。
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
			return ip.String()
		}
	}

	return ""
}

func isTrustedProxy(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

type clickRequest struct {
	ID *int `json:"id"` // 指针：区分"字段缺失"和"显式传 0"
}

type clickResponse struct {
	ID     int   `json:"id"`
	Clicks int64 `json:"clicks"`
	Total  int64 `json:"total"`
}

func (s *driverStore) clicksHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	if !s.limiter.allow(time.Now()) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	var payload clickRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxClickBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || payload.ID == nil || decoder.More() {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// 闭集校验。没有这一步，随便发随机 id 就能把 counts 和 clicks.json
	// 无界撑大，最终写满磁盘并连带搞挂更新器的 writeChunks。
	if !s.hasID(*payload.ID) {
		http.Error(w, "unknown driver", http.StatusBadRequest)
		return
	}

	key := clientIP(r)
	if key != "" {
		key += "|" + strconv.Itoa(*payload.ID)
	}

	var clicks, total int64
	if s.dedup.allow(key, time.Now()) {
		clicks, total = s.clicks.add(*payload.ID)
	} else {
		// 窗口内重复：不计数，但仍返回当前值，前端照常显示
		clicks, total = s.clicks.lookup(*payload.ID)
	}

	writeJSON(w, http.StatusOK, clickResponse{ID: *payload.ID, Clicks: clicks, Total: total})
}
