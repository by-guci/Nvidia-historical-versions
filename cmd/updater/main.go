package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================
// NVIDIA 驱动数据更新器
// 从 NVIDIA GeForce Service Toolkit API 抓取驱动数据
// ============================================

const (
	// 每次请求间隔
	requestDelay = 200 * time.Millisecond
)

// NVIDIA GeForce 服务 API，测试中会被替换为本地 httptest 地址
var apiBaseURL = "https://gfwsl.geforce.cn/services_toolkit/services/com/nvidia/services/AjaxDriverService.php"

// allowedLanguages 站点收录的语言，与 static/index.html 的语言下拉框保持一致。
// NVIDIA 对同一个驱动会按十几种语言各发一个 downloadID，这里只取站点实际展示的三种。
var allowedLanguages = map[string]bool{
	"Chinese (Simplified)":  true,
	"Chinese (Traditional)": true,
	"English (US)":          true,
}

// Driver 驱动数据结构
type Driver struct {
	ID            int    `json:"id"`
	DriverID      int    `json:"driver_id"`
	DriverName    string `json:"driver_name"`
	DriverVersion string `json:"driver_version"`
	ReleaseTime   string `json:"release_time"`
	Language      string `json:"language"`
	OS            string `json:"os"`
	DetailURL     string `json:"detail_url"`
	DownloadURL   string `json:"download_url"`
}

// apiResponse NVIDIA API 返回结构
type apiResponse struct {
	Success string      `json:"Success"`
	IDS     []idWrapper `json:"IDS"`
}

type idWrapper struct {
	DownloadInfo downloadInfo `json:"downloadInfo"`
}

type downloadInfo struct {
	Success         string   `json:"Success"`
	ID              string   `json:"ID"`
	Name            string   `json:"Name"`
	Version         string   `json:"Version"`
	ReleaseDateTime string   `json:"ReleaseDateTime"`
	DetailsURL      string   `json:"DetailsURL"`
	DownloadURL     string   `json:"DownloadURL"`
	LanguageName    string   `json:"LanguageName"`
	OSName          string   `json:"OSName"`
	OSList          []osItem `json:"OSList"`
	IsWHQL          string   `json:"IsWHQL"`
	IsBeta          string   `json:"IsBeta"`
	IsActive        string   `json:"IsActive"`
	IsDC            string   `json:"IsDC"`
}

type osItem struct {
	OSName string `json:"OSName"`
	OsCode string `json:"OsCode"`
}

func main() {
	dataDirFlag := flag.String("data", "static/data", "驱动数据目录路径")
	scanRange := flag.Int("scan", 3000, "从当前最大 ID 向前扫描的数量")
	workersFlag := flag.Int("workers", 5, "并发请求数")
	dryRun := flag.Bool("dry-run", false, "仅扫描不写入文件")
	flag.Parse()

	dataDir := *dataDirFlag

	// 加载现有数据
	existing, maxID, err := loadExistingData(dataDir)
	if err != nil {
		log.Fatalf("加载现有数据失败: %v", err)
	}
	log.Printf("已加载 %d 条现有驱动记录，最大 ID: %d", len(existing), maxID)

	// 扫描新驱动
	startID := maxID + 1
	if startID < 1 {
		startID = 1
	}
	endID := startID + *scanRange - 1

	log.Printf("扫描范围: ID %d → %d (%d 个)", startID, endID, *scanRange)
	newDrivers := scanDrivers(startID, endID, *workersFlag)
	log.Printf("发现 %d 条新驱动记录", len(newDrivers))

	if *dryRun {
		log.Println("Dry-run 模式，不写入文件")
		for _, d := range newDrivers {
			log.Printf("  [%d] %s v%s | %s | %s", d.ID, d.DriverName, d.DriverVersion, d.OS, d.Language)
		}
		return
	}

	if len(newDrivers) == 0 {
		log.Println("无新驱动，无需更新文件")
		return
	}

	// 合并数据
	allDrivers := mergeDrivers(existing, newDrivers)
	log.Printf("合并后总计 %d 条驱动记录", len(allDrivers))

	// 写入分片文件
	if err := writeChunks(allDrivers, dataDir); err != nil {
		log.Fatalf("写入数据失败: %v", err)
	}
	log.Println("更新完成!")
}

// loadExistingData 加载现有驱动数据。
// 只有 index.json 确实不存在时才按首次运行处理；其余读取/解析错误一律返回错误。
// 否则一次瞬时的读失败会被当成"空数据集"，随后用少量新数据覆盖掉整个数据集。
func loadExistingData(dataDir string) ([]Driver, int, error) {
	var all []Driver
	maxID := 0

	indexPath := filepath.Join(dataDir, "index.json")
	indexData, err := os.ReadFile(indexPath)
	if os.IsNotExist(err) {
		log.Printf("%s 不存在，按首次运行处理", indexPath)
		return all, maxID, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("读取 index.json: %w", err)
	}

	var index struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(indexData, &index); err != nil {
		return nil, 0, fmt.Errorf("解析 index.json: %w", err)
	}

	for _, file := range index.Files {
		if filepath.Base(file) != file {
			return nil, 0, fmt.Errorf("非法分片文件名 %q", file)
		}
		chunkPath := filepath.Join(dataDir, file)
		data, err := os.ReadFile(chunkPath)
		if err != nil {
			return nil, 0, fmt.Errorf("读取 %s: %w", file, err)
		}
		var chunk struct {
			Items []Driver `json:"items"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return nil, 0, fmt.Errorf("解析 %s: %w", file, err)
		}
		all = append(all, chunk.Items...)
		for _, d := range chunk.Items {
			if d.ID > maxID {
				maxID = d.ID
			}
		}
	}

	return all, maxID, nil
}

// scanDrivers 并发扫描指定 ID 范围的驱动
func scanDrivers(startID, endID, numWorkers int) []Driver {
	var (
		mu         sync.Mutex
		newDrivers []Driver
		found      atomic.Int64
		total      atomic.Int64
	)

	idChan := make(chan int, numWorkers*2)
	var wg sync.WaitGroup

	// 启动 worker
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 15 * time.Second}
			for id := range idChan {
				total.Add(1)
				driver, ok := fetchDriver(client, id)
				if ok {
					mu.Lock()
					newDrivers = append(newDrivers, driver)
					mu.Unlock()
					found.Add(1)
				}

				// 每 100 个 ID 打印进度
				if n := total.Load(); n%100 == 0 {
					log.Printf("进度: %d/%d | 已发现: %d",
						n, endID-startID+1, found.Load())
				}

				time.Sleep(requestDelay)
			}
		}()
	}

	// 发送 ID
	for id := startID; id <= endID; id++ {
		idChan <- id
	}
	close(idChan)
	wg.Wait()

	return newDrivers
}

// fetchDriver 从 NVIDIA API 获取单个驱动详情
func fetchDriver(client *http.Client, id int) (Driver, bool) {
	params := url.Values{}
	params.Set("func", "GetDownloadDetails")
	params.Set("downloadID", strconv.Itoa(id))

	reqURL := apiBaseURL + "?" + params.Encode()
	resp, err := client.Get(reqURL)
	if err != nil {
		return Driver{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Driver{}, false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return Driver{}, false
	}

	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return Driver{}, false
	}

	if apiResp.Success != "1" || len(apiResp.IDS) == 0 {
		return Driver{}, false
	}

	info := apiResp.IDS[0].DownloadInfo
	if info.Success != "1" || info.IsActive != "1" {
		return Driver{}, false
	}

	// 按语言收录。不能用 IsDC 过滤：IsDC 表示 DCH 封装，
	// Linux / FreeBSD / Solaris / Win7 等驱动的 IsDC 均为 0，
	// 用它过滤会丢掉现有数据集里约八成的记录。
	language := unescape(info.LanguageName)
	if !allowedLanguages[language] {
		return Driver{}, false
	}

	return Driver{
		ID:            id,
		DriverID:      id,
		DriverName:    unescape(info.Name),
		DriverVersion: info.Version,
		ReleaseTime:   parseDate(info.ReleaseDateTime),
		Language:      language,
		OS:            formatOSList(info.OSList),
		DetailURL:     info.DetailsURL,
		DownloadURL:   info.DownloadURL,
	}, true
}

// parseDate 解析 NVIDIA 日期格式 "Tue Jul 07, 2026" → "2026-07-07"
func parseDate(dateStr string) string {
	dateStr = strings.TrimSpace(dateStr)
	if dateStr == "" {
		return ""
	}
	t, err := time.Parse("Mon Jan 02, 2006", dateStr)
	if err != nil {
		// 尝试其他格式
		t, err = time.Parse("Mon Jan 2, 2006", dateStr)
		if err != nil {
			return dateStr
		}
	}
	return t.Format("2006-01-02")
}

// unescape URL 解码，忽略错误
func unescape(s string) string {
	decoded, err := url.QueryUnescape(s)
	if err != nil {
		return s
	}
	return decoded
}

// formatOSList 将 OSList 格式化为逗号分隔的字符串
func formatOSList(osList []osItem) string {
	if len(osList) == 0 {
		return ""
	}
	names := make([]string, 0, len(osList))
	seen := make(map[string]bool)
	for _, os := range osList {
		name := unescape(os.OSName)
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

// mergeDrivers 合并新旧驱动，去重并按 ID 降序排列
func mergeDrivers(existing, newDrivers []Driver) []Driver {
	seen := make(map[int]bool)
	var all []Driver

	for _, d := range existing {
		if !seen[d.ID] {
			seen[d.ID] = true
			all = append(all, d)
		}
	}
	for _, d := range newDrivers {
		if !seen[d.ID] {
			seen[d.ID] = true
			all = append(all, d)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].ID > all[j].ID
	})

	return all
}

// writeChunks 将驱动数据写入分片 JSON 文件。
// 任一分片写失败立即中止：若继续写 index.json，失败的分片不会出现在文件列表里，
// 整整一片（1000 条）记录会被静默丢弃且退出码仍为 0。
func writeChunks(drivers []Driver, dataDir string) error {
	chunkSize := 1000
	totalChunks := (len(drivers) + chunkSize - 1) / chunkSize

	fileNames := make([]string, 0, totalChunks)
	for i := 0; i < totalChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(drivers) {
			end = len(drivers)
		}

		fileName := fmt.Sprintf("drivers-%03d.json", i+1)

		chunk := struct {
			Items []Driver `json:"items"`
		}{Items: drivers[start:end]}

		data, err := json.MarshalIndent(chunk, "", "  ")
		if err != nil {
			return fmt.Errorf("序列化 %s: %w", fileName, err)
		}

		if err := writeFileAtomic(filepath.Join(dataDir, fileName), data); err != nil {
			return fmt.Errorf("写入 %s: %w", fileName, err)
		}

		fileNames = append(fileNames, fileName)
		log.Printf("写入 %s (%d 条)", fileName, end-start)
	}

	// index.json 最后写：服务端以它的 mtime 判断数据是否变更，
	// 先写完全部分片可以保证服务端重新加载时读到的是完整数据。
	index := struct {
		Files []string `json:"files"`
	}{Files: fileNames}

	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 index.json: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dataDir, "index.json"), indexData); err != nil {
		return fmt.Errorf("写入 index.json: %w", err)
	}
	log.Printf("写入 index.json (%d 个分片)", len(fileNames))

	removeStaleChunks(dataDir, fileNames)
	return nil
}

// writeFileAtomic 先写临时文件再 rename，避免进程中途退出留下半截 JSON
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

// removeStaleChunks 删除本次未写入、已不再被 index.json 引用的历史分片。
// 记录数减少或分片方案变化时，旧文件否则会一直堆在数据目录里。
func removeStaleChunks(dataDir string, current []string) {
	keep := make(map[string]bool, len(current))
	for _, name := range current {
		keep[name] = true
	}

	matches, err := filepath.Glob(filepath.Join(dataDir, "drivers-*.json"))
	if err != nil {
		log.Printf("扫描历史分片失败: %v", err)
		return
	}
	for _, match := range matches {
		name := filepath.Base(match)
		if keep[name] {
			continue
		}
		if err := os.Remove(match); err != nil {
			log.Printf("删除孤儿分片 %s 失败: %v", name, err)
			continue
		}
		log.Printf("删除孤儿分片 %s", name)
	}
}
