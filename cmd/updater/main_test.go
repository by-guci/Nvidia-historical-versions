package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 取自 NVIDIA API 真实返回的字段与转义形式（LanguageName / Name 均为 URL 编码）
func apiPayload(language, osName, isDC, isActive string) string {
	return fmt.Sprintf(`{ "Success" : "1", "IDS" : [{ "downloadInfo": {
		"Success" : "1", "ID" : "270362", "Name" : "NVIDIA%%20Studio%%20Driver",
		"Version" : "596.36", "ReleaseDateTime" : "Tue Apr 28, 2026",
		"DetailsURL" : "https://www.nvidia.com/zh-cn/drivers/details/270362/",
		"DownloadURL" : "https://cn.download.nvidia.com/x.exe",
		"LanguageName" : %q, "OSList" : [{ "OSName" : %q, "OsCode" : "1" }],
		"IsBeta" : "0", "IsWHQL" : "1", "IsDC" : %q, "IsActive" : %q }}]}`,
		language, osName, isDC, isActive)
}

func TestFetchDriverFiltersByLanguageNotIsDC(t *testing.T) {
	cases := []struct {
		name     string
		language string
		osName   string
		isDC     string
		isActive string
		want     bool
	}{
		// 回归用例：Linux/FreeBSD/Win7 等驱动 IsDC 恒为 0，
		// 旧的 `IsDC != "1"` 过滤会把它们全部丢掉。
		{"Linux 简体中文 IsDC=0", "Chinese%20(Simplified)", "Linux 64-bit", "0", "1", true},
		{"FreeBSD 美式英语 IsDC=0", "English%20(US)", "FreeBSD x64", "0", "1", true},
		{"Win7 繁体中文 IsDC=0", "Chinese%20(Traditional)", "Windows 7 64-bit", "0", "1", true},
		{"Win11 简体中文 IsDC=1", "Chinese%20(Simplified)", "Windows 11", "1", "1", true},

		// 非站点语言一律不收录
		{"德语", "Deutsch", "Windows 11", "1", "1", false},
		{"俄语", "%D0%A0%D1%83%D1%81%D1%81%D0%BA%D0%B8%D0%B9", "Windows 11", "1", "1", false},
		{"印度英语", "English%20(India)", "Linux 64-bit", "0", "1", false},

		// 已下架的驱动不收录
		{"简体中文但已下架", "Chinese%20(Simplified)", "Windows 11", "1", "0", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, apiPayload(tc.language, tc.osName, tc.isDC, tc.isActive))
			}))
			defer server.Close()

			original := apiBaseURL
			apiBaseURL = server.URL
			defer func() { apiBaseURL = original }()

			got, ok := fetchDriver(server.Client(), 270362)
			if ok != tc.want {
				t.Fatalf("fetchDriver() ok = %v, want %v", ok, tc.want)
			}
			if !ok {
				return
			}
			if got.OS != tc.osName {
				t.Errorf("OS = %q, want %q", got.OS, tc.osName)
			}
			if got.ReleaseTime != "2026-04-28" {
				t.Errorf("ReleaseTime = %q, want 2026-04-28", got.ReleaseTime)
			}
			if got.DriverName != "NVIDIA Studio Driver" {
				t.Errorf("DriverName = %q, want 解码后的名称", got.DriverName)
			}
		})
	}
}

func TestLoadExistingDataFailsLoudlyOnCorruptIndex(t *testing.T) {
	// index.json 不存在 = 首次运行，允许空数据集
	emptyDir := t.TempDir()
	drivers, maxID, err := loadExistingData(emptyDir)
	if err != nil || len(drivers) != 0 || maxID != 0 {
		t.Fatalf("首次运行应返回空数据集，得到 drivers=%d maxID=%d err=%v", len(drivers), maxID, err)
	}

	// index.json 损坏必须报错，不能当成空数据集——否则会用少量新数据覆盖整个数据集
	corruptDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(corruptDir, "index.json"), []byte("{ not json"), 0644); err != nil {
		t.Fatalf("准备损坏的 index.json: %v", err)
	}
	if _, _, err := loadExistingData(corruptDir); err == nil {
		t.Fatal("index.json 损坏时应返回错误，实际返回 nil")
	}

	// index.json 引用了不存在的分片，同样必须报错
	missingDir := t.TempDir()
	writeJSON(t, filepath.Join(missingDir, "index.json"), map[string][]string{"files": {"drivers-001.json"}})
	if _, _, err := loadExistingData(missingDir); err == nil {
		t.Fatal("分片缺失时应返回错误，实际返回 nil")
	}
}

func TestWriteChunksRemovesStaleFiles(t *testing.T) {
	dataDir := t.TempDir()

	// 预置一个上一轮遗留、本轮不会再写的分片
	stale := filepath.Join(dataDir, "drivers-007.json")
	if err := os.WriteFile(stale, []byte(`{"items":[]}`), 0644); err != nil {
		t.Fatalf("准备孤儿分片: %v", err)
	}

	drivers := make([]Driver, 1200) // 跨两个分片
	for i := range drivers {
		drivers[i] = Driver{ID: i + 1, DriverName: "d", Language: "English (US)"}
	}

	if err := writeChunks(drivers, dataDir); err != nil {
		t.Fatalf("writeChunks() error = %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("孤儿分片 drivers-007.json 应被删除，err = %v", err)
	}

	// 不应残留任何 .tmp 中间文件
	tmps, _ := filepath.Glob(filepath.Join(dataDir, "*.tmp"))
	if len(tmps) != 0 {
		t.Errorf("残留临时文件: %v", tmps)
	}

	// index.json 必须与实际写出的分片一致，且能被完整读回
	loaded, maxID, err := loadExistingData(dataDir)
	if err != nil {
		t.Fatalf("回读数据: %v", err)
	}
	if len(loaded) != 1200 || maxID != 1200 {
		t.Fatalf("回读结果 = %d 条 maxID=%d，want 1200 / 1200", len(loaded), maxID)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
