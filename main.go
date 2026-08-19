package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
	maxPageNumber   = 1000000
)

type driver struct {
	ID            int    `json:"id"`
	DriverID      int    `json:"driver_id"`
	DriverName    string `json:"driver_name"`
	DriverVersion string `json:"driver_version"`
	ReleaseTime   string `json:"release_time"`
	Language      string `json:"language"`
	OS            string `json:"os"`
	DetailURL     string `json:"detail_url"`
	DownloadURL   string `json:"download_url"`

	// 加载时预计算的小写检索串。未导出字段不参与 JSON 编解码，
	// 避免每个请求都对全部记录重复做 ToLower/Join。
	searchBlob   string
	versionLower string
}

type driverManifest struct {
	Files []string `json:"files"`
}

type driverChunk struct {
	Items []driver `json:"items"`
}

type driverOptions struct {
	OS        []string `json:"os"`
	Languages []string `json:"languages"`
}

type driverResponse struct {
	Items []driver `json:"items"`
	Total int      `json:"total"`
	// 计数与 items 平级，只含当页且计数大于 0 的条目
	Clicks      map[int]int64 `json:"clicks"`
	ClicksTotal int64         `json:"clicks_total"`
}

type dataFingerprint struct {
	modTime time.Time
	size    int64
}

type driverStore struct {
	dataDir     string
	mu          sync.RWMutex
	drivers     []driver
	ids         map[int]struct{} // 合法 driver ID 闭集，供计数接口校验
	options     driverOptions
	fingerprint dataFingerprint

	// 计数相关的状态独立于 drivers，不受 refreshIfNeeded 整体替换影响
	clicks  *clickCounter
	dedup   *clickDedup
	limiter *tokenBucket
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}

	staticDir := "./static"
	dataDir := filepath.Join(staticDir, "data")

	counter := newClickCounter(filepath.Join(dataDir, "clicks.json"))

	store, err := newDriverStore(dataDir, counter)
	if err != nil {
		log.Printf("initial driver data load failed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", store.healthHandler)
	mux.HandleFunc("/api/drivers", store.driversHandler)
	mux.HandleFunc("/api/options", store.optionsHandler)
	mux.HandleFunc("/api/clicks", store.clicksHandler)
	mux.Handle("/", staticGuard(http.FileServer(http.Dir(staticDir))))

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      withLogging(cacheControl(mux)),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("NVIDIA driver search listening on http://0.0.0.0:%s", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server: %v", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 顺序不能反：先停机让在途的计数请求跑完，再落盘。
	_ = srv.Shutdown(shutdownCtx)
	if err := counter.Close(); err != nil {
		log.Printf("ERROR 退出前点击计数落盘失败: %v", err)
	}
}

func newDriverStore(dataDir string, clicks *clickCounter) (*driverStore, error) {
	store := &driverStore{
		dataDir: dataDir,
		clicks:  clicks,
		dedup:   newClickDedup(clickDedupWindow),
		limiter: newTokenBucket(clickRatePerSecond, clickRateBurst),
	}
	return store, store.refreshIfNeeded()
}

// hasID 判断 id 是否存在于当前数据集。
//
// 必须加锁：refreshIfNeeded 会在写锁下把 s.ids 整体换成新 map。
// map 内容本身在 loadDriverData 里构造完才赋值、之后不再改动，
// 所以无锁读不会触发运行时的并发 map 检测——它是字段指针上的数据竞争，
// 只有 -race 能发现，实测去掉这两行本地测试照样全绿。
// 按 Go 内存模型这仍是未定义行为，锁不能省。
func (s *driverStore) hasID(id int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.ids[id]
	return ok
}

func (s *driverStore) refreshIfNeeded() error {
	fingerprint, err := manifestFingerprint(s.dataDir)
	if err != nil {
		return err
	}

	s.mu.RLock()
	upToDate := len(s.drivers) > 0 && s.fingerprint == fingerprint
	s.mu.RUnlock()
	if upToDate {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	fingerprint, err = manifestFingerprint(s.dataDir)
	if err != nil {
		return err
	}
	if len(s.drivers) > 0 && s.fingerprint == fingerprint {
		return nil
	}

	drivers, ids, options, err := loadDriverData(s.dataDir)
	if err != nil {
		if len(s.drivers) > 0 {
			log.Printf("driver cache reload failed, serving previous data: %v", err)
			return nil
		}
		return err
	}

	s.drivers = drivers
	s.ids = ids
	s.options = options
	s.fingerprint = fingerprint
	log.Printf("loaded %d driver records", len(drivers))
	return nil
}

func manifestFingerprint(dataDir string) (dataFingerprint, error) {
	info, err := os.Stat(filepath.Join(dataDir, "index.json"))
	if err != nil {
		return dataFingerprint{}, err
	}
	return dataFingerprint{modTime: info.ModTime(), size: info.Size()}, nil
}

// loadDriverData 返回记录、合法 ID 闭集、可选项。
// ID 闭集就是去重用的 seen map，计数接口靠它拒绝不存在的 id。
func loadDriverData(dataDir string) ([]driver, map[int]struct{}, driverOptions, error) {
	indexData, err := os.ReadFile(filepath.Join(dataDir, "index.json"))
	if err != nil {
		return nil, nil, driverOptions{}, err
	}

	var manifest driverManifest
	if err := json.Unmarshal(indexData, &manifest); err != nil {
		return nil, nil, driverOptions{}, fmt.Errorf("parse index: %w", err)
	}

	seen := make(map[int]struct{})
	drivers := make([]driver, 0, len(manifest.Files)*1000)
	osValues := make(map[string]struct{})
	languageValues := make(map[string]struct{})

	for _, name := range manifest.Files {
		if filepath.Base(name) != name {
			return nil, nil, driverOptions{}, fmt.Errorf("invalid data file name %q", name)
		}

		chunkData, err := os.ReadFile(filepath.Join(dataDir, name))
		if err != nil {
			return nil, nil, driverOptions{}, fmt.Errorf("read %s: %w", name, err)
		}

		var chunk driverChunk
		if err := json.Unmarshal(chunkData, &chunk); err != nil {
			return nil, nil, driverOptions{}, fmt.Errorf("parse %s: %w", name, err)
		}

		for _, item := range chunk.Items {
			if _, exists := seen[item.ID]; exists {
				continue
			}
			seen[item.ID] = struct{}{}
			drivers = append(drivers, item)
			if item.OS != "" {
				osValues[item.OS] = struct{}{}
			}
			if item.Language != "" {
				languageValues[item.Language] = struct{}{}
			}
		}
	}

	for i := range drivers {
		item := &drivers[i]
		item.versionLower = strings.ToLower(item.DriverVersion)
		item.searchBlob = strings.ToLower(strings.Join([]string{
			item.DriverName,
			item.DriverVersion,
			item.OS,
			item.Language,
		}, " "))
	}

	sort.Slice(drivers, func(i, j int) bool {
		if drivers[i].ReleaseTime == drivers[j].ReleaseTime {
			return drivers[i].ID > drivers[j].ID
		}
		return drivers[i].ReleaseTime > drivers[j].ReleaseTime
	})

	return drivers, seen, driverOptions{
		OS:        sortedValues(osValues),
		Languages: sortedValues(languageValues),
	}, nil
}

func sortedValues(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (s *driverStore) snapshot() ([]driver, driverOptions, error) {
	if err := s.refreshIfNeeded(); err != nil {
		return nil, driverOptions{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.drivers, s.options, nil
}

func (s *driverStore) driversHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	drivers, _, err := s.snapshot()
	if err != nil {
		http.Error(w, "driver data unavailable", http.StatusServiceUnavailable)
		return
	}

	query := r.URL.Query()
	keyword := strings.ToLower(strings.TrimSpace(query.Get("keyword")))
	version := strings.ToLower(strings.TrimSpace(query.Get("version")))
	osFilter := query.Get("os")
	languageFilter := query.Get("language")
	filtered := make([]driver, 0)

	for _, item := range drivers {
		if osFilter != "" && item.OS != osFilter {
			continue
		}
		if languageFilter != "" && item.Language != languageFilter {
			continue
		}
		if version != "" && !strings.Contains(item.versionLower, version) {
			continue
		}
		if keyword != "" && !strings.Contains(item.searchBlob, keyword) {
			continue
		}
		filtered = append(filtered, item)
	}

	page := queryInt(query.Get("page"), 1, 1, maxPageNumber)
	pageSize := queryInt(query.Get("pageSize"), defaultPageSize, 1, maxPageSize)
	start := (page - 1) * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}

	items := filtered[start:end]
	clicks, clicksTotal := s.clicks.pageCounts(items)
	writeJSON(w, http.StatusOK, driverResponse{
		Items:       items,
		Total:       len(filtered),
		Clicks:      clicks,
		ClicksTotal: clicksTotal,
	})
}

func (s *driverStore) optionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	_, options, err := s.snapshot()
	if err != nil {
		http.Error(w, "driver data unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, options)
}

func queryInt(value string, fallback, min, max int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < min {
		return fallback
	}
	if parsed > max {
		return max
	}
	return parsed
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// healthHandler 始终返回 200：docker-compose 的 healthcheck 打的就是这个端点，
// 返非 200 会让容器被反复重建。计数写盘是否正常通过字段暴露，不影响存活判定。
func (s *driverStore) healthHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"clicks_writable": s.clicks.healthy(),
	})
}

// staticGuard 关闭目录列表，并禁止直接访问原始数据文件。
// 前端只通过 /api 取数，/data 下的分片无需对外暴露。
func staticGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ServeMux 已对路径做过 clean，这里只需前缀判断
		if r.URL.Path == "/data" || strings.HasPrefix(r.URL.Path, "/data/") {
			http.NotFound(w, r)
			return
		}
		// 目录访问一律 404（根路径除外，由 FileServer 返回 index.html）
		if r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/api/"):
			w.Header().Set("Cache-Control", "no-cache")
		case strings.HasSuffix(path, ".js"):
			w.Header().Set("Cache-Control", "no-cache")
		case strings.HasSuffix(path, ".css"), strings.HasPrefix(path, "/assets/"):
			w.Header().Set("Cache-Control", "public, max-age=604800")
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, wrapped.status, time.Since(start).Round(time.Microsecond))
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
