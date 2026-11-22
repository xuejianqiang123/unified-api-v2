package dbserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestQRPollDoesNotSendCookies 守卫「确认请求不得回传出码时的 cookie」。
//
// 曾经的错误修复：以为网页版流程是「出码下发 csrf → 确认回传」，于是加了
// req.AddCookie，结果上游立刻从 status=new 变成 error_code=2035「非法请求」。
// 实测（未扫码状态逐组合对比）证明：不带 cookie 时任意参数组合都正常，
// 带 cookie 时全部 2035。该接口的 token 自带会话标识，额外带 csrf 会冲突。
func TestQRPollDoesNotSendCookies(t *testing.T) {
	var pollCookie string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/get"):
			http.SetCookie(w, &http.Cookie{Name: "passport_csrf_token", Value: "csrf-abc", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"message":"success","data":{"error_code":0,"token":"qr-token-1","qrcode":"data:image/png;base64,AAA"}}`))
		case strings.HasPrefix(r.URL.Path, "/check"):
			pollCookie = r.Header.Get("Cookie")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"message":"success","data":{"status":"waiting","description":""}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer up.Close()

	s := newTestServer(t, "http://127.0.0.1:1")
	s.cfg.QR.GetURL = up.URL + "/get"
	s.cfg.QR.CheckURL = up.URL + "/check"
	s.cfg.QR.Query = map[string]string{"aid": "582478"}

	// 出码
	rec := httptest.NewRecorder()
	s.qrStart(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/qr", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("出码失败: %d %s", rec.Code, rec.Body.String())
	}
	var startBody string = rec.Body.String()
	if !strings.Contains(startBody, `"id"`) {
		t.Fatalf("出码响应缺 id: %s", startBody)
	}
	// 从响应里取 id（简单提取，避免引入 json 依赖路径问题）
	id := ""
	for i := 0; i+7 < len(startBody); i++ {
		if startBody[i:i+6] == `"id":"` {
			j := i + 6
			for j < len(startBody) && startBody[j] != '"' {
				j++
			}
			id = startBody[i+6 : j]
			break
		}
	}
	if id == "" {
		t.Fatalf("未能解析出会话 id: %s", startBody)
	}

	// 轮询
	rec2 := httptest.NewRecorder()
	s.qrPoll(rec2, httptest.NewRequest(http.MethodGet, "/admin/accounts/qr?id="+id, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("轮询失败: %d %s", rec2.Code, rec2.Body.String())
	}
	if strings.Contains(pollCookie, "passport_csrf_token") {
		t.Fatalf("确认请求不应回传 cookie（会触发上游 2035 非法请求），实际 Cookie=%q", pollCookie)
	}
}
