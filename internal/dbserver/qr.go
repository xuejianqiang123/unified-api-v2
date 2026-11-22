package dbserver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	config "unified-api-v2/internal/dbconfig"
	pool "unified-api-v2/internal/dbpool"
)

// qrSession 一次扫码登录会话。
type qrSession struct {
	ID        string
	Token     string
	Status    string // waiting | scanned | confirmed | expired | error
	Message   string
	Image     string // data:image/png;base64,...（若有）
	QRURL     string
	Account   string
	CreatedAt time.Time
}

var (
	qrMu       sync.Mutex
	qrSess     = map[string]*qrSession{}
	acctFileMu sync.Mutex
)

// persistAccount 把扫码确认的账号落盘到 accounts.json，避免重启后丢号。
// 与面板 sync_accounts 写同一文件，用独立互斥锁防并发覆盖。
func (s *Server) persistAccount(a config.Account) {
	acctFileMu.Lock()
	defer acctFileMu.Unlock()
	path := s.accountsPath()
	var f accountsFile
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	for _, e := range f.Accounts {
		if e.Name == a.Name {
			return
		}
	}
	f.Accounts = append(f.Accounts, a)
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		logf("qr_persist_marshal_error account=%s err=%v", a.Name, err)
		return
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		logf("qr_persist_write_error account=%s err=%v", a.Name, err)
		return
	}
	logf("qr_persist account=%s file=%s", a.Name, path)
}

// qrStart 创建扫码会话：请求 passport 的 get_qrcode，返回二维码。
func (s *Server) qrStart(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.QR.Enabled || s.cfg.QR.GetURL == "" {
		writeErr(w, http.StatusNotImplemented,
			"扫码登录未启用：请在 config.json 配置 qr.get_url / qr.check_url（参数可从抓包得到）",
			"qr_disabled", "not_implemented")
		return
	}
	u, err := url.Parse(s.cfg.QR.GetURL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "qr.get_url 非法", "qr_config", "api_error")
		return
	}
	q := u.Query()
	for k, v := range s.cfg.QR.Query {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", s.cfg.Upstream.UserAgent)
	req.Header.Set("Referer", "https://www.doubao.com/")
	resp, err := s.http.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "请求二维码失败: "+err.Error(), "qr_upstream", "api_error")
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var payload struct {
		Message string `json:"message"`
		Data    struct {
			ErrorCode      int    `json:"error_code"`
			Description    string `json:"description"`
			Token          string `json:"token"`
			Qrcode         string `json:"qrcode"`
			Base64Image    string `json:"base64_image"`
			QrcodeIndexURL string `json:"qrcode_index_url"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &payload)
	if payload.Data.ErrorCode != 0 || (payload.Data.Token == "" && payload.Data.Qrcode == "") {
		msg := payload.Data.Description
		if msg == "" {
			msg = payload.Message
		}
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		writeErr(w, http.StatusBadGateway, "上游拒绝生成二维码: "+msg, "qr_rejected", "api_error")
		return
	}

	token := payload.Data.Token
	if token == "" {
		token = payload.Data.Qrcode
	}
	image := payload.Data.Base64Image
	if image == "" {
		image = payload.Data.Qrcode // 实测：qrcode 字段本身就是 data:image/png;base64,...
	}
	sess := &qrSession{
		ID:        randomHex(8),
		Token:     token,
		Status:    "waiting",
		Image:     image,
		QRURL:     payload.Data.QrcodeIndexURL,
		CreatedAt: time.Now(),
	}
	qrMu.Lock()
	qrSess[sess.ID] = sess
	// 清理 10 分钟前的会话
	for id, v := range qrSess {
		if time.Since(v.CreatedAt) > 10*time.Minute {
			delete(qrSess, id)
		}
	}
	qrMu.Unlock()

	logf("qr_start id=%s token_len=%d", sess.ID, len(token))
	writeJSON(w, http.StatusOK, map[string]any{
		"id": sess.ID, "status": sess.Status,
		"image": sess.Image, "qrcode_url": sess.QRURL,
	})
}

// qrPoll 轮询扫码状态；确认后把 Cookie 加入号池。
func (s *Server) qrPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	qrMu.Lock()
	sess, ok := qrSess[id]
	qrMu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "会话不存在或已过期", "qr_not_found", "invalid_request_error")
		return
	}

	u, err := url.Parse(s.cfg.QR.CheckURL)
	if err != nil || s.cfg.QR.CheckURL == "" {
		writeErr(w, http.StatusInternalServerError, "qr.check_url 未配置", "qr_config", "api_error")
		return
	}
	q := u.Query()
	for k, v := range s.cfg.QR.Query {
		q.Set(k, v)
	}
	q.Set("token", sess.Token)
	u.RawQuery = q.Encode()

	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", s.cfg.Upstream.UserAgent)
	req.Header.Set("Referer", "https://www.doubao.com/")
	// 刻意不回传出码时的 cookie。
	// 实测（2026-11-22，未扫码状态逐组合验证）：不带 cookie 时所有参数组合都返回
	// status=new；一旦回传 passport_csrf_token 就变成 error_code=2035「非法请求」。
	// 该接口的 token 参数自带会话标识，额外带 csrf 会与请求签名冲突。
	resp, err := s.http.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": sess.Status, "message": "轮询失败: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var payload struct {
		Message string `json:"message"`
		Data    struct {
			Status      string `json:"status"` // 实测为字符串：new / scanning / confirmed / expired
			Description string `json:"description"`
			RedirectURL string `json:"redirect_url"`
			ErrorCode   int    `json:"error_code"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &payload)

	status := mapQRStatus(payload.Data.Status)
	// 上游在参数不符时会回「非法请求 / 服务繁忙」这类 description，但 status 字段缺失，
	// 只保留 message 无法定位缺哪个参数。这里把原始响应记进日志（截断）。
	logf("qr_poll_raw id=%s status=%q desc=%q raw=%s", id, payload.Data.Status,
		payload.Data.Description, truncate(string(raw), 300))
	if status == "" {
		status = sess.Status
		if payload.Data.Description != "" {
			sess.Message = payload.Data.Description
		}
	}
	if payload.Data.RedirectURL != "" {
		sess.Message = payload.Data.RedirectURL
	}

	if status == "confirmed" {
		cookie := collectCookies(resp)
		if cookie != "" {
			name := "qr-" + randomHex(4)
			s.mu.Lock()
			acct := config.Account{Name: name, Cookie: cookie, LocalAppID: "582478", Note: "扫码新增"}
			s.accs[name] = acct
			s.mu.Unlock()
			s.pool.Add(pool.Key{Name: name, Key: name, Priority: 0})
			s.persistAccount(acct)
			sess.Account = name
			logf("qr_confirmed id=%s account=%s", id, name)
		}
	}

	qrMu.Lock()
	sess.Status = status
	qrMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "status": status, "message": sess.Message, "account": sess.Account,
	})
}

// collectCookies 把上游 Set-Cookie 拼成请求头格式。
func collectCookies(resp *http.Response) string {
	parts := []string{}
	for _, c := range resp.Cookies() {
		if c.Name == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", c.Name, c.Value))
	}
	return strings.Join(parts, "; ")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// mapQRStatus 把 passport 返回的状态（字符串/数字字符串）归一化成内部状态。
func mapQRStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "new", "1", "0", "waiting":
		return "waiting"
	case "scanning", "scanned", "2", "5":
		return "scanned"
	case "confirmed", "success", "3", "6":
		return "confirmed"
	case "expired", "4", "7":
		return "expired"
	case "canceled", "cancelled", "8":
		return "canceled"
	}
	return ""
}

// upsertAccount 落盘账号：同名则覆盖，不存在则追加。
//
// 与 persistAccount 的分工：后者遇到同名直接 return，只用于「扫码新增」；
// 而修改/导入 Cookie 必须覆盖旧值，否则面板改完重启又回滚。
func (s *Server) upsertAccount(a config.Account) {
	acctFileMu.Lock()
	defer acctFileMu.Unlock()
	path := s.accountsPath()
	var f accountsFile
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	replaced := false
	for i := range f.Accounts {
		if f.Accounts[i].Name == a.Name {
			f.Accounts[i] = a
			replaced = true
			break
		}
	}
	if !replaced {
		f.Accounts = append(f.Accounts, a)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		logf("account_upsert_marshal_error account=%s err=%v", a.Name, err)
		return
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		logf("account_upsert_write_error account=%s err=%v", a.Name, err)
	}
}

// cookieUID 从 cookie 串里取账号级标识（uid_tt 优先，兼容 __tea__ug__uid）。
func cookieUID(cookie string) string {
	for _, part := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "uid_tt":
			return strings.TrimSpace(kv[1])
		case "__tea__ug__uid":
			if v := strings.TrimSpace(kv[1]); v != "" {
				return v
			}
		}
	}
	return ""
}

// resolveImportTarget 决定导入时写哪条记录：同 uid 的已有账号优先复用。
//
// 为什么必须去重：同一个豆包账号可能已经以 main / 手写别名存在，若每次导入都
// 新增 local-xxxxxxxx，号池会堆出一串同一账号的记录，只能手工删——这正是用户
// 反馈的问题。按 uid_tt 认人，已存在就更新它的 cookie，不产生新记录。
func (s *Server) resolveImportTarget(name, uid string) string {
	// 同名直接复用（重复导入同一 profile 的常见路径）。
	if _, ok := s.accs[name]; ok {
		return name
	}
	if uid == "" {
		return name
	}
	// 多账号同 uid 时取名字序最小的，保证结果稳定（map 遍历顺序随机）。
	matches := make([]string, 0, 2)
	for existing, rec := range s.accs {
		if cookieUID(rec.Cookie) == uid {
			matches = append(matches, existing)
		}
	}
	if len(matches) == 0 {
		return name
	}
	sort.Strings(matches)
	return matches[0]
}

// adminImportLocalCookie 从本机豆包客户端导入登录态（替代手工抓 cookie）。
//
// 遍历客户端的所有 profile：同一客户端可同时登录多个豆包账号（Default 与
// Profile 1 的 sessionid 不同），只读 Default 会漏号。账号名由 uid_tt 派生
// （local-xxxxxxxx），重复导入同一账号是覆盖而非新增。
func (s *Server) adminImportLocalCookie(w http.ResponseWriter, r *http.Request) {
	res, err := ImportDoubaoLocalCookies()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "local_import_failed", "invalid_request_error")
		return
	}
	type imported struct {
		Name    string `json:"name"`
		Profile string `json:"profile"`
		Keys    int    `json:"keys"`
		Added   bool   `json:"added"`
	}
	out := make([]imported, 0, len(res.Accounts))
	for _, p := range res.Accounts {
		// 同 uid 已存在则复用其记录名，避免同一账号堆出多条。
		target := s.resolveImportTarget(p.Name, p.UID)
		s.mu.Lock()
		a, existed := s.accs[target]
		a.Name = target
		a.Cookie = p.Cookie
		if a.LocalAppID == "" {
			a.LocalAppID = "582478"
		}
		a.Note = "本地客户端导入 · " + p.Profile
		s.accs[p.Name] = a
		s.mu.Unlock()

		if !existed {
			s.pool.Add(pool.Key{Name: a.Name, Key: a.Name, Priority: a.Priority})
		}
		s.upsertAccount(a)
		out = append(out, imported{Name: target, Profile: p.Profile, Keys: p.Keys, Added: !existed})
	}
	logf("local_cookie_import count=%d", len(out))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "imported": out, "skipped": res.Skipped, "user_data": res.UserData,
	})
}

// adminRemoveAccount 真正删除账号：内存 + 池 + accounts.json 一并移除。
//
// 与 adminDelete（DELETE /admin/accounts）的区别：后者只把账号移出池、
// 记录仍留在文件里（设计初衷是"停用"）。这里用于清理重复/废弃账号，
// 必须连记录一起删，否则重启后又回到池子里。
func (s *Server) adminRemoveAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "参数错误", "bad_json", "invalid_request_error")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空", "bad_request", "invalid_request_error")
		return
	}
	s.mu.Lock()
	_, ok := s.accs[name]
	if ok {
		delete(s.accs, name)
	}
	s.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name, "not_found", "invalid_request_error")
		return
	}
	s.pool.Remove(name)

	acctFileMu.Lock()
	path := s.accountsPath()
	var f accountsFile
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	kept := f.Accounts[:0]
	for _, e := range f.Accounts {
		if e.Name != name {
			kept = append(kept, e)
		}
	}
	f.Accounts = kept
	raw, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		if werr := os.WriteFile(path, raw, 0o600); werr != nil {
			logf("account_remove_write_error account=%s err=%v", name, werr)
		}
	}
	acctFileMu.Unlock()

	logf("account_removed name=%s", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": name, "total": s.pool.Len()})
}
