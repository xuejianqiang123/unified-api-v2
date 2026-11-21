package dbserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdminCheckinLogHistoryNewestFirst 守护豆包签到历史的读取语义。
//
// 背景（2026-11-21 用户反馈「为什么把豆包签到删除了」）：豆包签到结果此前只在
// 内存 map（lastCheckin，重启清零）里，面板只读了 WorkBuddy 的记录，豆包历史
// 虽然一直在写 usage_checkin.jsonl 却没有任何读取入口。
//
// 判据：
//  1. 从 JSONL 读出的记录按时间倒序（最新在前）——用户明确要求倒序；
//  2. limit 生效，且只截断尾部（保留最新的 N 条）；
//  3. 坏行被跳过而不是整表失败；
//  4. 文件不存在时返回空数组 + 200，而不是报错。
func TestAdminCheckinLogHistoryNewestFirst(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "usage.jsonl")
	t.Setenv("DOUBAO2API_USAGE", base)
	path := strings.TrimSuffix(base, ".jsonl") + "_checkin.jsonl"

	lines := []string{
		`{"name":"main","status":"ok","message":"+10 积分","ts":1000}`,
		`{"name":"main","status":"already","message":"今日已签","ts":2000}`,
		`{ this is not json`, // 坏行必须被跳过
		`{"name":"main","status":"no_campaign","message":"当前无可用签到活动","ts":3000}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	s := newTestServerWithUpstream(t, "http://127.0.0.1:1", nil)

	req := httptest.NewRequest(http.MethodGet, "/admin/checkin/log?limit=2", nil)
	req.RemoteAddr = "127.0.0.1:0" // 通过 loopback 管理闸
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Records []struct {
			Status string `json:"status"`
			TS     int64  `json:"ts"`
		} `json:"records"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(out.Records) != 2 {
		t.Fatalf("limit=2 应返回 2 条，实际 %d 条：%+v", len(out.Records), out.Records)
	}
	// 最新在前：ts 必须严格递减，且第一条是 3000
	if out.Records[0].TS != 3000 || out.Records[1].TS != 2000 {
		t.Fatalf("未按时间倒序（或 limit 截断错端）：%+v", out.Records)
	}
	if out.Records[0].TS <= out.Records[1].TS {
		t.Fatalf("ts 不是递减：%d, %d", out.Records[0].TS, out.Records[1].TS)
	}
}

// TestAdminCheckinLogMissingFileIsEmpty 文件不存在（从未签到）时必须是空数组 + 200。
func TestAdminCheckinLogMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOUBAO2API_USAGE", filepath.Join(dir, "usage.jsonl"))

	s := newTestServerWithUpstream(t, "http://127.0.0.1:1", nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/checkin/log", nil)
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Records []any `json:"records"`
		Total   int   `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 0 || len(out.Records) != 0 {
		t.Fatalf("期望空结果，实际 total=%d records=%d", out.Total, len(out.Records))
	}
}
