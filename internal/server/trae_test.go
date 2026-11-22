package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"unified-api-v2/internal/pool"
)

// TestTraeChatProxiesAndRewritesHeaders 验证聊天反代：路径、请求体、鉴权头、响应都按预期透传。
//
// Trae 是独立 Python 进程，网关不能像 doubao 那样直接调函数——转发层出错会表现为
// 「模型能列出但一调用就 502」，所以这里用 httptest 模拟上游做端到端断言。
func TestTraeChatProxiesAndStreams(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer up.Close()

	c := NewTraeClient(up.URL, "sk-trae-test")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
	c.Chat(rec, req)

	if gotPath != "/v1/chat/completions" {
		t.Errorf("上游路径 = %q，期望 /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-trae-test" {
		t.Errorf("鉴权头 = %q，期望 Bearer sk-trae-test", gotAuth)
	}
	if !strings.Contains(string(gotBody), "glm-5.3") {
		t.Errorf("上游收到的 body 不含模型名：%s", gotBody)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("下游状态 = %d，期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Errorf("下游 body 未透传：%s", rec.Body.String())
	}
}

// TestTraeChatUpstreamDown 上游不可达时必须是明确的 502，而不是空响应或 200。
func TestTraeChatUpstreamDown(t *testing.T) {
	c := NewTraeClient("http://127.0.0.1:1", "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	c.Chat(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态 = %d，期望 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "upstream_unreachable") {
		t.Errorf("错误体缺少 upstream_unreachable：%s", rec.Body.String())
	}
}

// TestTraeAdminWhitelist 管理代理只放通白名单，且挡住路径穿越。
//
// 这条守卫的意义：面板能调用的 Trae 端点必须是我们明确放通的（账号/签到），
// 不能因为「反代写得太宽容」把 /api/logout、/api/settings、/web/login 暴露出去。
func TestTraeAdminWhitelist(t *testing.T) {
	c := NewTraeClient("http://127.0.0.1:1", "")
	cases := []struct {
		method, path string
		wantStatus   int
		why          string
	}{
		{http.MethodGet, "/api/logout", http.StatusForbidden, "登出不在白名单"},
		{http.MethodGet, "/api/settings", http.StatusForbidden, "设置不在白名单"},
		{http.MethodGet, "/web/login", http.StatusForbidden, "登录页不在白名单"},
		{http.MethodGet, "/v1/chat/completions", http.StatusForbidden, "聊天接口不走管理代理"},
		{http.MethodPost, "/api/checkin/account/../remove", http.StatusForbidden, "路径穿越必须被挡住"},
		{http.MethodPost, "/api/checkin/account/a/b", http.StatusForbidden, "多段路径不是合法账号 ID"},
	}
	for _, tc := range cases {
		_, st := c.Admin(context.Background(), tc.method, tc.path, nil)
		if st != tc.wantStatus {
			t.Errorf("%s %s → %d，期望 %d（%s）", tc.method, tc.path, st, tc.wantStatus, tc.why)
		}
	}
	// 白名单内的 GET 应放通到网络层（此处上游不可达，预期 502 而不是 403）
	if _, st := c.Admin(context.Background(), http.MethodGet, "/api/checkin/accounts", nil); st == http.StatusForbidden {
		t.Error("白名单端点被错误拒绝")
	}
}

// TestTraeSafeIDSegment 账号 ID 段校验。
func TestTraeSafeIDSegment(t *testing.T) {
	ok := []string{"main", "2091742335607939", "a-b_c"}
	bad := []string{"", "a/b", "../x", "a b", "a.b", strings.Repeat("x", 129)}
	for _, s := range ok {
		if !traeSafeIDSegment(s) {
			t.Errorf("%q 应被接受", s)
		}
	}
	for _, s := range bad {
		if traeSafeIDSegment(s) {
			t.Errorf("%q 应被拒绝", s)
		}
	}
}

// TestProviderStatusCountsThreeProviders 验证三方别名归属统计。
//
// 旧实现用「是否含 workbuddy」判断公共/独有，加入 trae 后会把只含
// doubao+trae 的公共别名错算成 workbuddy 的独有别名。这里用能暴露该错误的组合。
func TestProviderStatusCountsThreeProviders(t *testing.T) {
	h := NewHandler(Config{
		Pool:     pool.New(""),
		TraeChat: func(w http.ResponseWriter, r *http.Request) {},
		Aliases: map[string]UnifiedAlias{
			// 三方公共
			"triple": {Providers: []string{"doubao", "workbuddy", "trae"}, Failover: true, Model: "m1"},
			// 两方公共，不含 workbuddy —— 旧逻辑会把它算成 workbuddy 独有
			"db-trae": {Providers: []string{"doubao", "trae"}, Failover: true, Model: "m2"},
			// trae 独有
			"trae-only": {Provider: "trae", Model: "m3"},
		},
	})
	st := h.providerStatus()

	get := func(name string) map[string]any {
		m, _ := st[name].(map[string]any)
		if m == nil {
			t.Fatalf("providerStatus 缺 %s", name)
		}
		return m
	}
	num := func(m map[string]any, k string) int {
		v, ok := m[k]
		if !ok {
			t.Fatalf("缺字段 %s", k)
		}
		f, _ := v.(int)
		return f
	}

	tr := get("trae")
	if num(tr, "aliases_public") != 2 || num(tr, "aliases_own") != 1 || num(tr, "aliases") != 3 {
		t.Errorf("trae 统计 = public:%d own:%d aliases:%d，期望 2/1/3",
			num(tr, "aliases_public"), num(tr, "aliases_own"), num(tr, "aliases"))
	}
	wb := get("workbuddy")
	if num(wb, "aliases_public") != 1 || num(wb, "aliases_own") != 0 {
		t.Errorf("workbuddy 统计 = public:%d own:%d，期望 1/0", num(wb, "aliases_public"), num(wb, "aliases_own"))
	}
	db := get("doubao")
	if num(db, "aliases_public") != 2 || num(db, "aliases_own") != 0 {
		t.Errorf("doubao 统计 = public:%d own:%d，期望 2/0", num(db, "aliases_public"), num(db, "aliases_own"))
	}
}

// TestDispatchSingleProviderTraeIsWired 守卫「单 provider」路径的 trae 接线。
//
// 背景（本轮真实踩到）：dispatchUnifiedAlias 的单 provider 分支与 callProvider 的
// 多候选分支是两段独立实现。加 trae 时只接了后者，于是 provider:"trae" 的别名
// 请求直接返回 501「provider "trae" 尚未接入统一网关」，而公共别名（多候选）
// 却工作正常——这种"一半能用"的故障最容易漏测。
//
// 判据：TraeChat 必须被调用、body 里的模型名被重写成上游名、X-Unified-Alias 回显别名。
func TestDispatchSingleProviderTraeIsWired(t *testing.T) {
	called := false
	h := NewHandler(Config{
		Pool: pool.New(""),
		Aliases: map[string]UnifiedAlias{
			"trae-probe": {Provider: "trae", Model: "glm-5.3"},
		},
		TraeChat: func(w http.ResponseWriter, r *http.Request) {
			called = true
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "glm-5.3") {
				t.Errorf("上游收到的 body 未重写为上游模型名：%s", body)
			}
			if got := r.Header.Get("X-Unified-Alias"); got != "trae-probe" {
				t.Errorf("X-Unified-Alias = %q，期望 trae-probe", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"trae-probe","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatalf("TraeChat 未被调用（单 provider 路径漏接），状态=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", rec.Code)
	}
}

// TestFailoverReachesTraeAsThirdCandidate 验证三候选故障转移：前两家失败后落到 trae。
//
// 这是「公共别名第三候选」这个决定的核心判据：只验证单 provider 路径不足以说明
// 路由链能走到 trae，必须证明 doubao/workbuddy 都失败时 trae 会被调用且结果返回给客户端。
func TestFailoverReachesTraeAsThirdCandidate(t *testing.T) {
	var order []string
	h := NewHandler(Config{
		Pool: pool.New(""),
		Aliases: map[string]UnifiedAlias{
			"auto": {
				Providers: []string{"doubao", "workbuddy", "trae"},
				Failover:  true,
				Model:     "glm-5.3",
			},
		},
		DoubaoChat: func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "doubao")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"doubao down"}}`))
		},
		WorkbuddyChat: func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "workbuddy")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"wb down"}}`))
		},
		TraeChat: func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "trae")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from-trae"}}]}`))
		},
		DoubaoHealth: func() (int, int) { return 1, 1 },
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（trae 成功后应回放其结果）；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "from-trae") {
		t.Errorf("响应不是来自 trae：%s", rec.Body.String())
	}
	doubaoIdx, wbIdx, traeIdx := -1, -1, -1
	for i, p := range order {
		switch p {
		case "doubao":
			doubaoIdx = i
		case "workbuddy":
			wbIdx = i
		case "trae":
			traeIdx = i
		}
	}
	if traeIdx < 0 {
		t.Fatalf("trae 未被调用，实际调用顺序=%v", order)
	}
	if doubaoIdx >= 0 && wbIdx >= 0 && traeIdx >= 0 && !(doubaoIdx < wbIdx && wbIdx < traeIdx) {
		t.Errorf("调用顺序应为 doubao→workbuddy→trae，实际=%v", order)
	}
}

// TestTraeChatRecordsActivity 守卫「请求日志里能看到 Trae」这条链路。
//
// 背景：活动日志原先只在 chatStat.done() 里写 provider=workbuddy，
// 走 callProvider 的 trae 请求完全不进日志——面板的「请求日志」看起来像
// 只有 WorkBuddy 一家在跑。本测试断言 TraeClient.Chat 会写入一条 trae 记录。
func TestTraeChatRecordsActivity(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer up.Close()

	c := NewTraeClient(up.URL, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
	req.Header.Set("X-Unified-Alias", "auto")
	c.Chat(rec, req)

	var found *activityEntry
	for _, e := range wbActivity.snapshot(20) {
		if e.Provider == "trae" {
			v := e
			found = &v
			break
		}
	}
	if found == nil {
		t.Fatal("活动日志里没有 provider=trae 的记录（面板请求日志会看不到 Trae）")
	}
	if found.Status != http.StatusOK {
		t.Errorf("status=%d，期望 200", found.Status)
	}
	if found.Model != "glm-5.3" {
		t.Errorf("model=%q，期望 glm-5.3", found.Model)
	}
	if found.Alias != "auto" {
		t.Errorf("alias=%q，期望 auto", found.Alias)
	}
}
