package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
	"unified-api-v2/internal/vault"
)

// 网关侧固定使用的上游参数。
const (
	// DefaultBaseURL 是 snap-access 网关基址（来自 IDE 的 product.json）。
	DefaultBaseURL = "https://snap-access.cn-north-4.myhuaweicloud.com"
	// ChatPath 是聊天端点（OpenAI 兼容形态）。
	ChatPath = "/api/v2/chat/completions"
	// QuotaPath 是套餐与积分端点（裸对象，无 data 信封）。
	QuotaPath = "/snap-manager/v1/statistics/plugin"
	// agentType 必须与路由匹配：值错会得到「未找到合法可路由api」而不是 401，
	// 症状像「端点不存在」。chat 实测 AgentCenter 可用。
	agentType = "AgentCenter"
	// userAgent 必须是官方 IDE 形态。以 deepseek-harness/ 开头会被 WAF 403，
	// 而 403 在上层会被显示成「密钥无效」—— 一个看起来像凭据问题的 WAF 问题。
	userAgent = "codearts/1.0.0"
	// MaxTokensCap 是上游接受的上界，超出直接 400。
	MaxTokensCap = 65536
	// MaxTokensDefault 是 IDE 的客户端预算（未显式传 max_tokens 时用）。
	MaxTokensDefault = 32000
)

// ErrNoCredential 表示没有可用的 CodeArts 凭据。
var ErrNoCredential = errors.New("codearts: 没有可用凭据")

// Client 是 CodeArts 的进程内上游客户端。
//
// 凭据**每次请求都重读**：IDE 自己在后台续期并把新 STS 写回 secretStorage，
// 网关读同一份来源即可自动跟上，不必自己持有 refresh_token（那个是一次性轮换的，
// 抢来用会让 IDE 掉登录）。
type Client struct {
	credPath string
	baseURL  string
	http     *http.Client

	mu   sync.RWMutex
	last Credential
}

// NewClient 构造客户端。credPath 为凭据 JSON 路径（为空则不可用）。
func NewClient(credPath string) *Client {
	return &Client{
		credPath: credPath,
		baseURL:  DefaultBaseURL,
		http:     &http.Client{Timeout: 10 * time.Minute},
	}
}

// Credential 返回当前生效的凭据（读盘失败时回落到上一次成功的副本）。
func (c *Client) Credential() (Credential, error) {
	if c == nil || c.credPath == "" {
		return Credential{}, ErrNoCredential
	}
	// 走 vault：密文自动解密，明文原样返回（升级不需要迁移老文件）。
	raw, err := vault.ReadFile(c.credPath)
	if err != nil {
		c.mu.RLock()
		last := c.last
		c.mu.RUnlock()
		if last.HasSTS() {
			return last, nil
		}
		return Credential{}, fmt.Errorf("%w: %v", ErrNoCredential, err)
	}
	cred, err := ParseCredentialFile(raw)
	if err != nil {
		return Credential{}, err
	}
	c.mu.Lock()
	c.last = cred
	c.mu.Unlock()
	return cred, nil
}

// ParseCredentialFile 解析账号文件，兼容三种形态：
//  1. 单对象（本包 Credential 的 JSON）
//  2. {"accounts":[ ... ]}（多账号）
//  3. 数组（同上）
//
// 多账号时**选 STS 到期最晚的那个**，不是第一个：导入工具曾经因为 name 字段没写回
// 而把同一次登录追加成两条（旧的在先、新的在后），按「第一个」取会一直用到快过期
// 的那份，表现为「刚导入就报凭据过期」。
func ParseCredentialFile(raw []byte) (Credential, error) {
	var one Credential
	if json.Unmarshal(raw, &one) == nil && one.HasSTS() {
		return one, nil
	}
	var multi struct {
		Accounts []Credential `json:"accounts"`
	}
	if json.Unmarshal(raw, &multi) == nil {
		if best, ok := latestSTS(multi.Accounts); ok {
			return best, nil
		}
	}
	var arr []Credential
	if json.Unmarshal(raw, &arr) == nil {
		if best, ok := latestSTS(arr); ok {
			return best, nil
		}
	}
	return Credential{}, fmt.Errorf("%w: 凭据文件里没有含 AK/SK/security_token 的账号", ErrNoCredential)
}

// latestSTS 在多个账号里挑 STS 到期最晚的一个（到期时间缺失的排在最后）。
func latestSTS(list []Credential) (Credential, bool) {
	var best Credential
	var bestAt time.Time
	found := false
	for _, a := range list {
		if !a.HasSTS() {
			continue
		}
		if !found {
			best, found = a, true
			bestAt = parseExpiry(a.ExpiresAt)
			continue
		}
		at := parseExpiry(a.ExpiresAt)
		if at.After(bestAt) {
			best, bestAt = a, at
		}
	}
	return best, found
}

// parseExpiry 解析 RFC3339；解析不出返回零值（排序时视为最旧）。
func parseExpiry(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Do 发一次已签名的上游请求。body 为 nil 表示 GET。
//
// 这里 agent-type **进签名**：chat 与 statistics 实测这样才被接受。
func (c *Client) Do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	return c.doInternal(ctx, method, path, body, map[string]string{"agent-type": agentType}, true)
}

// opsPromptCenter 是 /v1/ops/* 与 /v1/model/* 的路由头。
//
// ⚠️ 这批端点与 chat 是**两套口径**：Agent-Type 必须**签名后追加**，
// 参与 canonical 会被判 401 verify ak sk signature fail（实测记录见上游笔记）。
const (
	opsAgentType = "PromptCenter"
	opsLanguage  = "zh-cn"
)

// DoOps 用于 /v1/ops/* 与 /v1/model/*：路由头只在签名后追加，不进 canonical。
func (c *Client) DoOps(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	return c.doInternal(ctx, method, path, body,
		map[string]string{"Agent-Type": opsAgentType, "X-Language": opsLanguage}, false)
}

func (c *Client) doInternal(ctx context.Context, method, path string, body []byte, extra map[string]string, signExtra bool) (*http.Response, error) {
	cred, err := c.Credential()
	if err != nil {
		return nil, err
	}
	full := c.baseURL + path
	if body == nil {
		body = []byte{}
	}
	signHeaders := extra
	if !signExtra {
		signHeaders = nil
	}
	headers, err := SignRequest(cred.AccessKeyID, cred.SecretAccessKey, cred.SecurityToken,
		method, full, body, signHeaders)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if method != http.MethodGet {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if !signExtra {
		// 这些头不参与 canonical，但服务端要靠它们选响应形态。
		for k, v := range extra {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// Chat 发一次聊天请求，返回**原始**响应（上层决定流式透传还是读错误）。
// body 必须是已按 CodeArts 形态组装好的 JSON。
func (c *Client) Chat(ctx context.Context, body []byte) (*http.Response, error) {
	return c.Do(ctx, http.MethodPost, ChatPath, body)
}

// Quota 取套餐与积分（裸对象形态）。供面板与 /v1/quota 使用。
func (c *Client) Quota(ctx context.Context) (map[string]any, error) {
	resp, err := c.Do(ctx, http.MethodGet, QuotaPath, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codearts: 额度查询 HTTP %d: %s", resp.StatusCode, truncateForErr(string(raw)))
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("codearts: 额度响应不是 JSON: %v", err)
	}
	return obj, nil
}

// QuotaSummary 是从 metrics[] 里提取出的额度视图。
//
// 口径（实测 4600 = 基础 500 + 赠送 4100）：
//   - 总额只认 usageTotalPackageCredit，**不累加各分类** —— 分类是总额的构成明细；
//   - 临期量只在总额 metric 上取一次（实测分类上也有同名字段，累加会翻倍）。
type QuotaSummary struct {
	Total         int64
	Basic         int64
	OnDemand      int64
	Bonus         int64
	Expiring      int64
	ExpiringKnown bool
	PackageName   string
}

// Summarize 把额度响应映射成定长视图。缺 key 与值为 0 严格区分。
func Summarize(obj map[string]any) QuotaSummary {
	var s QuotaSummary
	metrics, _ := obj["metrics"].([]any)
	for _, m := range metrics {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		name, _ := mm["name"].(string)
		remain := toInt64(mm["package_credit_remain"])
		switch name {
		case "usageTotalPackageCredit":
			s.Total = remain
			if v, ok := mm["package_credit_expiring_amount"]; ok {
				s.Expiring = toInt64(v)
				s.ExpiringKnown = true
			}
		case "usageBasicPackageCredit":
			s.Basic = remain
		case "usageOnDemandPackageCredit":
			s.OnDemand = remain
		case "usageBonusPackageCredit":
			s.Bonus = remain
		}
	}
	if pkg, ok := obj["package"].(map[string]any); ok {
		s.PackageName, _ = pkg["package_name_cn"].(string)
	}
	return s
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func truncateForErr(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "…"
}
