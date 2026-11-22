// usagestats.go 请求与 token 的累计用量（今日 / 累计），落盘 data/usage.json。
//
// 为什么单独做一份：activityStore 是进程内环形缓冲、重启清零，只能看「刚才」；
// 用户要看的是「今天用了多少 / 总共用了多少」，必须持久化。
package server

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

type usageData struct {
	Day         string `json:"day"`          // 当前统计日（YYYY-MM-DD，本地时区）
	TodayReq    int64  `json:"today_req"`    // 今日请求数
	TodayTokens int64  `json:"today_tokens"` // 今日 token 数
	TotalReq    int64  `json:"total_req"`    // 累计请求数
	TotalTokens int64  `json:"total_tokens"` // 累计 token 数
	Since       string `json:"since"`        // 首次统计时间，便于说明「累计从何时起」
}

type usageStats struct {
	mu    sync.Mutex
	path  string
	data  usageData
	dirty bool
}

// usageGlobal 包级单例：recordActivity 是全局函数，没有 Handler 可依赖。
var usageGlobal = newUsageStats("data/usage.json")

func newUsageStats(path string) *usageStats {
	u := &usageStats{path: path}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &u.data)
	}
	if u.data.Since == "" {
		u.data.Since = time.Now().Format(time.RFC3339)
	}
	return u
}

// addReq 记一次请求（**全路径**：在 chatCompletions 入口统一计一次）。
func (u *usageStats) addReq() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rollLocked()
	u.data.TodayReq++
	u.data.TotalReq++
	u.dirty = true
}

// addTokens 累加 token。只统计能观测到 usage 的路径（workbuddy / trae），
// 其余 provider 目前拿不到用量 —— 不编造，宁可数字偏小。
func (u *usageStats) addTokens(tokens int) {
	if tokens <= 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rollLocked()
	u.data.TodayTokens += int64(tokens)
	u.data.TotalTokens += int64(tokens)
	u.dirty = true
}

// rollLocked 跨天时把「今日」归零，累计不清。
func (u *usageStats) rollLocked() {
	today := time.Now().Format("2006-01-02")
	if u.data.Day != today {
		u.data.Day = today
		u.data.TodayReq = 0
		u.data.TodayTokens = 0
	}
}

// Snapshot 返回副本，并顺手落盘（面板打开时才写，避免把 IO 压到请求路径上）。
func (u *usageStats) Snapshot() usageData {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.dirty {
		u.flushLocked()
	}
	return u.data
}

func (u *usageStats) flushLocked() {
	if u.path == "" {
		return
	}
	raw, err := json.MarshalIndent(u.data, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll("data", 0o755); err != nil {
		return
	}
	if err := os.WriteFile(u.path, raw, 0o644); err != nil {
		log.Printf("用量统计落盘失败: %v", err)
		return
	}
	u.dirty = false
}
