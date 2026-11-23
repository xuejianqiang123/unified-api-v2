package dbserver

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// checkinResult 单个账号的签到结果。
type checkinResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok / already / no_campaign / error
	Message string `json:"message"`
	Reward  string `json:"reward,omitempty"`
	Days    int    `json:"days,omitempty"`
	TS      int64  `json:"ts"`
}

var (
	checkinMu   sync.RWMutex
	lastCheckin = map[string]checkinResult{}
)

// doCheckin 调豆包日常签到接口。
//
// 接口：POST /alice/ug/campaign/landing/checkin
// 报文：{"scene": "<scene>", "biz_params": {}}   ← biz_params 必须是对象，传字符串会 400
func (s *Server) runCheckinAll(only string) []checkinResult {
	type acct struct{ name, cookie, dev string }
	var list []acct
	s.mu.Lock()
	for n, a := range s.accs {
		if only != "" && n != only {
			continue
		}
		list = append(list, acct{n, a.Cookie, a.DeviceID})
	}
	s.mu.Unlock()

	out := make([]checkinResult, 0, len(list))
	for _, a := range list {
		r := s.doCheckin(a.name, a.cookie, a.dev)
		checkinMu.Lock()
		lastCheckin[a.name] = r
		checkinMu.Unlock()
		s.appendCheckinLog(r)
		logf("checkin acct=%s status=%s msg=%s", r.Name, r.Status, r.Message)
		out = append(out, r)
	}
	return out
}

// appendCheckinLog 追加到 checkin.jsonl。
func (s *Server) appendCheckinLog(r checkinResult) {
	line, _ := json.Marshal(r)
	f, err := os.OpenFile(strings.TrimSuffix(s.usagePath(), ".jsonl")+"_checkin.jsonl",
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = f.Write(append(line, '\n'))
		f.Close()
	}
}

// checkinLoop 每天在配置的整点执行一次签到。
func (s *Server) checkinLoop() {
	if !s.cfg.Checkin.Enabled {
		return
	}
	hour := s.cfg.Checkin.Hour
	if hour <= 0 || hour > 23 {
		hour = 9 // 默认每天 9 点
	}
	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		time.Sleep(time.Until(next))
		res := s.runCheckinAll("")
		logf("checkin_daily done accounts=%d", len(res))
	}
}

// adminCheckin 手动触发签到：?name= 指定单个账号，缺省全部。
func (s *Server) adminCheckin(w http.ResponseWriter, r *http.Request) {
	res := s.runCheckinAll(r.URL.Query().Get("name"))
	writeJSON(w, http.StatusOK, map[string]any{"results": res, "total": len(res)})
}

// adminCheckinStatus 查看最近一次签到结果。
func (s *Server) adminCheckinStatus(w http.ResponseWriter, r *http.Request) {
	checkinMu.RLock()
	out := make([]checkinResult, 0, len(lastCheckin))
	for _, v := range lastCheckin {
		out = append(out, v)
	}
	checkinMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"results":     out,
		"enabled":     s.cfg.Checkin.Enabled,
		"hour":        s.cfg.Checkin.Hour,
		"scene":       s.cfg.Checkin.Scene,
		"server_time": time.Now().Format("2006-01-02 15:04:05"),
	})
}

// formatReward 把积分数量格式化成文本。
func formatReward(n int) string {
	if n <= 0 {
		return ""
	}
	return itoaSimple(n) + " 积分"
}

// adminCheckinLog 读取豆包签到历史（JSONL，最新在前）。
//
// 内存里的 lastCheckin 只保留每个账号的最新一条，且进程重启即清零；
// 真正的历史在 appendCheckinLog 写的 usage_checkin.jsonl 里。面板要展示
// 「豆包签到记录（倒序）」必须读文件，而不是读内存快照。
func (s *Server) adminCheckinLog(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	path := strings.TrimSuffix(s.usagePath(), ".jsonl") + "_checkin.jsonl"
	f, err := os.Open(path)
	if err != nil {
		// 文件不存在＝还没执行过签到，不是错误。
		writeJSON(w, http.StatusOK, map[string]any{"records": []checkinResult{}, "total": 0, "path": path})
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	recs := make([]checkinResult, 0, 64)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var one checkinResult
		if json.Unmarshal([]byte(line), &one) != nil {
			continue // 跳过坏行，不因一行损坏导致整表不可用
		}
		recs = append(recs, one)
	}
	// 文件是追加写的（旧→新），倒序成全表最新在前。
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	if len(recs) > limit {
		recs = recs[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": recs, "total": len(recs), "path": path})
}
