package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDriverEndpoints(t *testing.T) {
	dataDir := t.TempDir()
	writeTestJSON(t, filepath.Join(dataDir, "index.json"), driverManifest{Files: []string{"drivers-001.json"}})
	writeTestJSON(t, filepath.Join(dataDir, "drivers-001.json"), driverChunk{Items: []driver{
		{ID: 1, DriverName: "Game Ready", DriverVersion: "610.74", ReleaseTime: "2026-07-07", Language: "Chinese (Simplified)", OS: "Windows 11"},
		{ID: 2, DriverName: "Studio", DriverVersion: "610.62", ReleaseTime: "2026-06-16", Language: "Chinese (Simplified)", OS: "Windows 11"},
		{ID: 3, DriverName: "Linux Driver", DriverVersion: "610.62", ReleaseTime: "2026-06-16", Language: "English (US)", OS: "Linux"},
	}})

	store := newTestStore(t, dataDir)

	driversRequest := httptest.NewRequest(http.MethodGet, "/api/drivers?language=Chinese%20%28Simplified%29&page=1&pageSize=1", nil)
	driversResponse := httptest.NewRecorder()
	store.driversHandler(driversResponse, driversRequest)
	if driversResponse.Code != http.StatusOK {
		t.Fatalf("drivers status = %d", driversResponse.Code)
	}

	var driversPayload driverResponse
	if err := json.NewDecoder(driversResponse.Body).Decode(&driversPayload); err != nil {
		t.Fatalf("decode drivers response: %v", err)
	}
	if driversPayload.Total != 2 || len(driversPayload.Items) != 1 || driversPayload.Items[0].ID != 1 {
		t.Fatalf("unexpected driver response: %#v", driversPayload)
	}

	optionsRequest := httptest.NewRequest(http.MethodGet, "/api/options", nil)
	optionsResponse := httptest.NewRecorder()
	store.optionsHandler(optionsResponse, optionsRequest)
	if optionsResponse.Code != http.StatusOK {
		t.Fatalf("options status = %d", optionsResponse.Code)
	}

	var optionsPayload driverOptions
	if err := json.NewDecoder(optionsResponse.Body).Decode(&optionsPayload); err != nil {
		t.Fatalf("decode options response: %v", err)
	}
	if len(optionsPayload.OS) != 2 || len(optionsPayload.Languages) != 2 {
		t.Fatalf("unexpected options response: %#v", optionsPayload)
	}
}

func TestProjectDriverDataLoads(t *testing.T) {
	store := newTestStore(t, filepath.Join("static", "data"))

	drivers, options, err := store.snapshot()
	if err != nil {
		t.Fatalf("snapshot() error = %v", err)
	}
	if len(drivers) == 0 || len(options.OS) == 0 || len(options.Languages) == 0 {
		t.Fatalf("project data did not load: drivers=%d os=%d languages=%d", len(drivers), len(options.OS), len(options.Languages))
	}
}

func TestStaticGuardBlocksDataAndListings(t *testing.T) {
	staticDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(staticDir, "data"), 0755); err != nil {
		t.Fatalf("prepare data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "data", "drivers-001.json"), []byte(`{"items":[]}`), 0644); err != nil {
		t.Fatalf("prepare chunk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "app.js"), []byte("// app"), 0644); err != nil {
		t.Fatalf("prepare app.js: %v", err)
	}

	handler := staticGuard(http.FileServer(http.Dir(staticDir)))

	blocked := []string{"/data/", "/data/drivers-001.json", "/data", "/assets/"}
	for _, path := range blocked {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, recorder.Code)
		}
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("GET /app.js status = %d, want 200", recorder.Code)
	}
}

// newTestStore 建一个 store，计数文件放临时目录，绝不写进项目的 static/data。
func newTestStore(t *testing.T, dataDir string) *driverStore {
	t.Helper()
	counter := newClickCounter(filepath.Join(t.TempDir(), "clicks.json"))
	t.Cleanup(func() { _ = counter.Close() })

	store, err := newDriverStore(dataDir, counter)
	if err != nil {
		t.Fatalf("newDriverStore() error = %v", err)
	}
	return store
}

// seedStore 建一个含三条记录（ID 1/2/3）的 store。
func seedStore(t *testing.T) *driverStore {
	t.Helper()
	dataDir := t.TempDir()
	writeTestJSON(t, filepath.Join(dataDir, "index.json"), driverManifest{Files: []string{"drivers-001.json"}})
	writeTestJSON(t, filepath.Join(dataDir, "drivers-001.json"), driverChunk{Items: []driver{
		{ID: 1, DriverName: "Game Ready", DriverVersion: "610.74", ReleaseTime: "2026-07-07", Language: "English (US)", OS: "Windows 11"},
		{ID: 2, DriverName: "Studio", DriverVersion: "610.62", ReleaseTime: "2026-06-16", Language: "English (US)", OS: "Windows 11"},
		{ID: 3, DriverName: "Linux", DriverVersion: "610.62", ReleaseTime: "2026-06-16", Language: "English (US)", OS: "Linux"},
	}})
	return newTestStore(t, dataDir)
}

func postClick(store *driverStore, body string, contentType string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/clicks", strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	store.clicksHandler(recorder, request)
	return recorder
}

func countedIDs(c *clickCounter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.counts)
}

func TestClickHandlerRejectsBadInput(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
	}{
		{"GET 不允许", http.MethodGet, "application/json", `{"id":1}`, http.StatusMethodNotAllowed},
		{"缺 Content-Type", http.MethodPost, "", `{"id":1}`, http.StatusUnsupportedMediaType},
		{"错误 Content-Type", http.MethodPost, "text/plain", `{"id":1}`, http.StatusUnsupportedMediaType},
		{"带 charset 的 json 应放行", http.MethodPost, "application/json; charset=utf-8", `{"id":1}`, http.StatusOK},
		{"空对象", http.MethodPost, "application/json", `{}`, http.StatusBadRequest},
		{"未知字段", http.MethodPost, "application/json", `{"id":1,"x":2}`, http.StatusBadRequest},
		{"多个 JSON 文档", http.MethodPost, "application/json", `{"id":1}{"id":2}`, http.StatusBadRequest},
		{"超大请求体", http.MethodPost, "application/json", `{"id":1,"pad":"` + strings.Repeat("A", 2048) + `"}`, http.StatusBadRequest},
		{"不存在的 ID", http.MethodPost, "application/json", `{"id":999999}`, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := seedStore(t)

			request := httptest.NewRequest(tc.method, "/api/clicks", strings.NewReader(tc.body))
			if tc.contentType != "" {
				request.Header.Set("Content-Type", tc.contentType)
			}
			recorder := httptest.NewRecorder()
			store.clicksHandler(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if recorder.Code == http.StatusMethodNotAllowed && recorder.Header().Get("Allow") != http.MethodPost {
				t.Errorf("Allow 头 = %q, want POST", recorder.Header().Get("Allow"))
			}
			// 最关键的一条：被拒的请求绝不能在 counts 里建桶，
			// 否则随机 id 就能把内存和 clicks.json 无界撑大。
			if tc.wantStatus != http.StatusOK && countedIDs(store.clicks) != 0 {
				t.Errorf("被拒请求污染了计数表: %d 个键", countedIDs(store.clicks))
			}
		})
	}
}

func TestClickHandlerCountsAndDedups(t *testing.T) {
	store := seedStore(t)

	recorder := postClick(store, `{"id":2}`, "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("首次点击 status = %d", recorder.Code)
	}
	var payload clickResponse
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.ID != 2 || payload.Clicks != 1 || payload.Total != 1 {
		t.Fatalf("首次点击响应 = %#v, want id=2 clicks=1 total=1", payload)
	}

	// 同一 IP（httptest 默认 RemoteAddr 固定）+ 同一驱动，窗口内不应再计
	recorder = postClick(store, `{"id":2}`, "application/json")
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Clicks != 1 || payload.Total != 1 {
		t.Fatalf("窗口内重复点击被计数了: %#v", payload)
	}

	// 换一个驱动仍应计数
	recorder = postClick(store, `{"id":3}`, "application/json")
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Clicks != 1 || payload.Total != 2 {
		t.Fatalf("换驱动后 = %#v, want clicks=1 total=2", payload)
	}
}

func TestClientIPTrustsProxyHeadersOnlyFromPrivateRemote(t *testing.T) {
	cases := []struct {
		name         string
		remoteAddr   string
		realIP       string
		forwardedFor string
		want         string
	}{
		{"直连", "203.0.113.9:5555", "", "", "203.0.113.9"},
		{"直连时伪造的代理头一律忽略", "203.0.113.9:5555", "1.2.3.4", "5.6.7.8", "203.0.113.9"},
		{"反代 + X-Real-IP", "127.0.0.1:5555", "203.0.113.9", "", "203.0.113.9"},
		{"反代 + XFF 取最右侧", "172.17.0.1:5555", "", "1.2.3.4, 203.0.113.9", "203.0.113.9"},
		{"反代 + 两者都有时优先 X-Real-IP", "10.0.0.1:5555", "203.0.113.9", "1.2.3.4", "203.0.113.9"},
		{"反代但未透传真实 IP", "127.0.0.1:5555", "", "", ""},
		{"反代 + XFF 是垃圾值", "127.0.0.1:5555", "", "garbage", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/clicks", nil)
			request.RemoteAddr = tc.remoteAddr
			if tc.realIP != "" {
				request.Header.Set("X-Real-IP", tc.realIP)
			}
			if tc.forwardedFor != "" {
				request.Header.Set("X-Forwarded-For", tc.forwardedFor)
			}
			if got := clientIP(request); got != tc.want {
				t.Fatalf("clientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClickDedupFailsOpenWithoutClientIP(t *testing.T) {
	dedup := newClickDedup(time.Minute)
	now := time.Now()
	// key 为空 = 拿不到访客 IP。必须每次都放行，
	// 否则反代未透传真实 IP 时所有访客会被当成同一个人。
	for i := 0; i < 5; i++ {
		if !dedup.allow("", now) {
			t.Fatal("空 key 被拒了，这会让反代场景下 99% 的点击静默消失")
		}
	}
}

func TestClickPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clicks.json")

	counter := newClickCounter(path)
	counter.add(270362)
	counter.add(270362)
	counter.add(224493)
	if err := counter.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened := newClickCounter(path)
	t.Cleanup(func() { _ = reopened.Close() })

	clicks, total := reopened.lookup(270362)
	if clicks != 2 {
		t.Errorf("270362 计数 = %d, want 2", clicks)
	}
	// total 不落盘、靠启动时求和重建。漏了这一步的症状是
	// 每行数字都对，但"全站总数"重启后回 0。
	if total != 3 {
		t.Errorf("总数 = %d, want 3", total)
	}
}

func TestClickCorruptFileIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clicks.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0644); err != nil {
		t.Fatalf("prepare corrupt file: %v", err)
	}

	counter := newClickCounter(path)
	t.Cleanup(func() { _ = counter.Close() })

	if _, total := counter.lookup(1); total != 0 {
		t.Errorf("损坏文件后总数 = %d, want 0", total)
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "clicks.json.corrupt.*"))
	if len(matches) != 1 {
		t.Fatalf("损坏文件应被隔离保留，实际匹配 %d 个", len(matches))
	}
}

func TestDriversResponseCarriesClicks(t *testing.T) {
	store := seedStore(t)
	store.clicks.add(1)
	store.clicks.add(1)

	recorder := httptest.NewRecorder()
	store.driversHandler(recorder, httptest.NewRequest(http.MethodGet, "/api/drivers?pageSize=10", nil))

	var payload driverResponse
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Clicks[1] != 2 {
		t.Errorf("clicks[1] = %d, want 2", payload.Clicks[1])
	}
	if payload.ClicksTotal != 2 {
		t.Errorf("clicks_total = %d, want 2", payload.ClicksTotal)
	}
	// 读路径不得为计数为 0 的记录建桶
	if _, exists := payload.Clicks[2]; exists {
		t.Errorf("响应里出现了计数为 0 的条目: %#v", payload.Clicks)
	}
	if countedIDs(store.clicks) != 1 {
		t.Errorf("读路径污染了计数表: %d 个键，want 1", countedIDs(store.clicks))
	}
}

// TestClickConcurrency 压计数器与数据热重载的并发路径。
//
// 改写 index.json 是必须的：指纹不变时 refreshIfNeeded 会直接早退，
// s.ids 根本不会被替换，这个测试就成了摆设。
//
// 实测能抓到的：clickCounter 的锁——去掉 add 的互斥锁立刻
// fatal error: concurrent map iteration and map write（flush 遍历时 add 在写）。
// 抓不到的：hasID 的 RLock。那是字段指针上的数据竞争，
// 运行时的并发 map 检测看不见，只有 go test -race 能发现。
func TestClickConcurrency(t *testing.T) {
	dataDir := t.TempDir()
	writeTestJSON(t, filepath.Join(dataDir, "drivers-001.json"), driverChunk{Items: []driver{
		{ID: 1, DriverName: "A", ReleaseTime: "2026-07-07", Language: "English (US)", OS: "Windows 11"},
		{ID: 2, DriverName: "B", ReleaseTime: "2026-07-06", Language: "English (US)", OS: "Windows 11"},
	}})
	writeTestJSON(t, filepath.Join(dataDir, "drivers-002.json"), driverChunk{Items: []driver{
		{ID: 3, DriverName: "C", ReleaseTime: "2026-07-05", Language: "English (US)", OS: "Linux"},
		{ID: 4, DriverName: "D", ReleaseTime: "2026-07-04", Language: "English (US)", OS: "Linux"},
	}})
	writeTestJSON(t, filepath.Join(dataDir, "index.json"), driverManifest{Files: []string{"drivers-001.json"}})

	store := newTestStore(t, dataDir)

	indexPath := filepath.Join(dataDir, "index.json")
	stop := make(chan struct{})
	var workers, rewriter sync.WaitGroup

	// 交替改写 index.json：两个 manifest 长度不同 → 指纹必变 →
	// refreshIfNeeded 真的重载并整体替换 s.ids
	rewriter.Add(1)
	go func() {
		defer rewriter.Done()
		manifests := []driverManifest{
			{Files: []string{"drivers-001.json"}},
			{Files: []string{"drivers-001.json", "drivers-002.json"}},
		}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if data, err := json.Marshal(manifests[i%2]); err == nil {
				_ = os.WriteFile(indexPath, data, 0644)
			}
		}
	}()

	const clickers, readers, iterations = 50, 10, 200
	for worker := 0; worker < clickers; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < iterations; i++ {
				store.clicks.add(1)
			}
		}()
	}
	for worker := 0; worker < readers; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < iterations; i++ {
				store.hasID(1)
				_ = store.refreshIfNeeded()
				_ = store.clicks.flush()
			}
		}()
	}

	workers.Wait()
	close(stop)
	rewriter.Wait()

	if _, total := store.clicks.lookup(1); total != clickers*iterations {
		t.Fatalf("并发后总数 = %d, want %d", total, clickers*iterations)
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal test data: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write test data: %v", err)
	}
}
