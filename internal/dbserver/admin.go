package dbserver

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	config "unified-api-v2/internal/dbconfig"
	pool "unified-api-v2/internal/dbpool"
)

// accountsFile 账号文件（面板写入后调用 /admin/reload 热加载）。
type accountsFile struct {
	Accounts []config.Account `json:"accounts"`
}

func (s *Server) accountsPath() string {
	if p := os.Getenv("DOUBAO2API_ACCOUNTS"); p != "" {
		return p
	}
	// 默认与 config.json 同目录
	exe, err := os.Executable()
	if err != nil {
		return "accounts.json"
	}
	return filepath.Join(filepath.Dir(exe), "accounts.json")
}

// adminToggleAccount 统一面板账号启停：只改进程内号池标志，不写 accounts.json。
// 请求体 {"disabled": true|false}；省略时按当前状态取反。
func (s *Server) adminToggleAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "缺少账号名", "bad_request", "invalid_request_error")
		return
	}
	var body struct {
		Disabled *bool `json:"disabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	_, exists := s.accs[name]
	s.mu.Unlock()
	if !exists {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name, "account_not_found", "invalid_request_error")
		return
	}
	var want bool
	if body.Disabled != nil {
		want = *body.Disabled
	} else {
		// 省略时取反：从当前状态推导，默认置为停用。
		want = true
		for _, st := range s.pool.Stats() {
			if st.Name == name {
				want = !st.Disabled
				break
			}
		}
	}
	if !s.pool.SetDisabled(name, want) {
		writeErr(w, http.StatusNotFound, "账号不在号池: "+name, "account_not_found", "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "disabled": want, "ok": true})
}

// adminListAccounts 返回账号名与可用状态（不下发 Cookie）。
func (s *Server) adminListAccounts(w http.ResponseWriter, r *http.Request) {
	stats := s.pool.Stats()
	out := make([]map[string]any, 0, len(stats))
	for _, st := range stats {
		item := map[string]any{
			"name":     st.Name,
			"cooling":  st.Cooling,
			"disabled": st.Disabled,
			"fails":    st.Fails,
			"reason":   st.Reason,
		}
		if a, ok := s.accs[st.Name]; ok {
			item["device_id"] = a.DeviceID
			item["note"] = a.Note
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out, "healthy": s.pool.Healthy(), "total": s.pool.Len()})
}

// adminReload 从 accounts.json 热加载账号（只增不减，避免误删在途请求）。
func (s *Server) adminReload(w http.ResponseWriter, r *http.Request) {
	path := s.accountsPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取账号文件失败: "+err.Error(), "accounts_file_error", "invalid_request_error")
		return
	}
	var f accountsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadRequest, "账号文件格式错误: "+err.Error(), "accounts_parse_error", "invalid_request_error")
		return
	}

	added := []string{}
	s.mu.Lock()
	for i := range f.Accounts {
		a := f.Accounts[i]
		a.Name = strings.TrimSpace(a.Name)
		if a.Name == "" || a.Cookie == "" {
			continue
		}
		if a.LocalAppID == "" {
			a.LocalAppID = "582478"
		}
		if _, exists := s.accs[a.Name]; exists {
			continue // 已存在：不覆盖（Cookie 更新走重启或同名替换策略）
		}
		s.accs[a.Name] = a
		s.pool.Add(pool.Key{Name: a.Name, Key: a.Name, Priority: a.Priority})
		added = append(added, a.Name)
	}
	s.mu.Unlock()

	logf("admin_reload file=%s added=%d total=%d", path, len(added), s.pool.Len())
	writeJSON(w, http.StatusOK, map[string]any{
		"added": added, "total": s.pool.Len(), "healthy": s.pool.Healthy(),
	})
}

// adminSetCookie 更新某账号 Cookie（面板改 Cookie 后调用）。
func (s *Server) adminSetCookie(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "参数错误", "bad_json", "invalid_request_error")
		return
	}
	if body.Name == "" || body.Cookie == "" {
		writeErr(w, http.StatusBadRequest, "name/cookie 不能为空", "bad_request", "invalid_request_error")
		return
	}
	s.mu.Lock()
	a, ok := s.accs[body.Name]
	if ok {
		a.Cookie = body.Cookie
		s.accs[body.Name] = a
	} else {
		a = config.Account{Name: body.Name, Cookie: body.Cookie, LocalAppID: "582478"}
		s.accs[body.Name] = a
		s.pool.Add(pool.Key{Name: a.Name, Key: a.Name, Priority: a.Priority})
	}
	s.mu.Unlock()
	// 原实现只改内存不落盘：面板改/加 Cookie 后一重启就回滚。
	s.upsertAccount(a)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": body.Name})
}

// adminDelete 停用某账号（从池中移除；Cookie 保留在文件里由面板管理）。
func (s *Server) adminDelete(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "缺少 name", "bad_request", "invalid_request_error")
		return
	}
	s.mu.Lock()
	delete(s.accs, name)
	s.mu.Unlock()
	s.pool.Remove(name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": name, "total": s.pool.Len()})
}

// loadAccountsFile 启动时把 accounts.json 里的账号装进池子（重启不丢号）。
func (s *Server) loadAccountsFile() {
	raw, err := os.ReadFile(s.accountsPath())
	if err != nil {
		return
	}
	var f accountsFile
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	n := 0
	s.mu.Lock()
	for i := range f.Accounts {
		a := f.Accounts[i]
		if a.Name == "" || a.Cookie == "" {
			continue
		}
		if a.LocalAppID == "" {
			a.LocalAppID = "582478"
		}
		if _, exists := s.accs[a.Name]; exists {
			continue
		}
		s.accs[a.Name] = a
		s.pool.Add(pool.Key{Name: a.Name, Key: a.Name, Priority: a.Priority})
		n++
	}
	s.mu.Unlock()
	if n > 0 {
		logf("startup_accounts loaded=%d total=%d", n, s.pool.Len())
	}
}

// adminAccountOrder 按给定顺序重排豆包账号优先级（数字小者优先）。
// 同时改内存号池与 accounts.json 持久化；names 必须恰好包含全部账号一次。
func (s *Server) adminAccountOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Names []string `json:"names"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || len(req.Names) == 0 {
		writeErr(w, http.StatusBadRequest, "参数错误: names 必须是非空数组", "bad_request", "invalid_request_error")
		return
	}
	ok, order := s.pool.SetPriorityOrder(req.Names)
	if !ok {
		writeErr(w, http.StatusBadRequest, "names 必须恰好包含每个账号一次", "invalid_request", "invalid_request_error")
		return
	}
	// 同步优先级到内存中的 accs，再写回 accounts.json（原子写：tmp + rename）。
	s.mu.Lock()
	prio := map[string]int{}
	for i, n := range order {
		prio[n] = i + 1
		if a, exists := s.accs[n]; exists {
			a.Priority = prio[n]
			s.accs[n] = a
		}
	}
	path := s.accountsPath()
	var f accountsFile
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	for i := range f.Accounts {
		if p, exists := prio[f.Accounts[i].Name]; exists {
			f.Accounts[i].Priority = p
		}
	}
	var writeErrValue error
	if len(f.Accounts) > 0 {
		raw, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			writeErrValue = err
		} else {
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, raw, 0o600); err != nil {
				writeErrValue = err
			} else {
				writeErrValue = os.Rename(tmp, path)
			}
		}
	}
	s.mu.Unlock()
	if writeErrValue != nil {
		// 号池优先级已生效，但持久化失败：如实告知，不回滚内存（回滚会让用户以为操作没发生）。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "order": order, "persisted": false,
			"warning": "优先级已在内存生效，但写入 accounts.json 失败: " + writeErrValue.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "order": order, "persisted": true})
}
