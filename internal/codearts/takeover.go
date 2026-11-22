package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Takeover 模式：网关自己持有 refresh_token 并续期。
//
// ⚠️ 与「只读模式」互斥：refresh_token 一次性轮换，网关刷新成功后，
// IDE 手里那份立即作废 —— 你下次打开码道 IDE 需要重新登录。
// 换取的是：不再依赖 IDE 存活，STS 过期能自愈。
type tokenResponse struct {
	Credentials struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
		Expiration      string `json:"expiration"`
	} `json:"credentials"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorCode    string `json:"error_code"`
	ErrorMsg     string `json:"error_msg"`
}

// ErrRefreshTerminal 表示 refresh_token 本身已失效，重试无意义（需要重新登录）。
var ErrRefreshTerminal = fmt.Errorf("codearts: refresh_token 已失效，需要重新登录")

// RefreshOwn 用 refresh_token 换一套新凭据，并写回账号文件。
//
// 判定要点（照官方客户端 + 实测量纲）：
//   - 只有 `invalid_grant` / 含 `ExpiredRefreshToken` / `STS5.1806`（一次性 token
//     已被消费）才算**终态**，其余一律按可重试失败处理；
//   - `InvalidDPoPHeader` **不是**终态：那是一次请求层面的拒绝（时钟偏差/重放），
//     refresh_token 本身还好着，把它当终态会让账号被永久标死。
func (c *Client) RefreshOwn(ctx context.Context) error {
	cred, err := c.Credential()
	if err != nil {
		return err
	}
	if cred.RefreshToken == "" {
		return fmt.Errorf("codearts: 没有 refresh_token，无法接管续期")
	}
	if cred.DpopPrivateJWK == nil {
		return fmt.Errorf("codearts: 没有 DPoP 私钥，无法接管续期")
	}

	dpop, err := signDPoP(cred.DpopPrivateJWK, http.MethodPost, stsTokenEndpoint)
	if err != nil {
		return err
	}

	form := url.Values{
		"client_id":     {oauthClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {cred.RefreshToken},
	}
	// PKCE verifier 与当初登录时配对：缺了它服务端会拒。
	if cred.CodeVerifier != "" {
		form.Set("code_verifier", cred.CodeVerifier)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stsTokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", dpop)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("codearts: 续期请求网络错误: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return fmt.Errorf("codearts: 续期响应不是 JSON（HTTP %d）: %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || tr.Credentials.SecurityToken == "" {
		msg := tr.ErrorMsg
		if msg == "" {
			msg = tr.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		if isTerminalRefreshError(tr.Error, tr.ErrorCode, msg) {
			return fmt.Errorf("%w: %s (%s)", ErrRefreshTerminal, msg, tr.ErrorCode)
		}
		return fmt.Errorf("codearts: 续期失败（可重试）: %s (%s)", msg, tr.ErrorCode)
	}

	// 成功：把新凭据写回（含轮换后的 refresh_token）。
	next := cred
	next.AccessKeyID = tr.Credentials.AccessKeyID
	next.SecretAccessKey = tr.Credentials.SecretAccessKey
	next.SecurityToken = tr.Credentials.SecurityToken
	if tr.RefreshToken != "" {
		next.RefreshToken = tr.RefreshToken
	}
	if exp := tr.Credentials.Expiration; exp != "" {
		next.ExpiresAt = normalizeExpiry(exp)
	}
	if err := SaveCredential(c.credPath, next); err != nil {
		// 写盘失败很危险：服务端已轮换，旧的立刻失效，新的必须落地。
		return fmt.Errorf("codearts: 续期成功但写盘失败（凭据可能已失效，请立即重新登录）: %w", err)
	}
	c.mu.Lock()
	c.last = next
	c.mu.Unlock()
	return nil
}

// isTerminalRefreshError 只认 refresh_token 自己失效的信号。
//
// ⚠️ 实测（2026-11-22）：`STS5.1806` **不止一个原因** ——
// 传错 client_id 时它同样返回 STS5.1806，但报文是
// "invalid client id: codearts"，那是一次**请求参数**问题，refresh_token 还好着。
// 只看错误码会把好账号标死（正是上游笔记里反复踩过的坑），所以必须看报文。
func isTerminalRefreshError(errStr, code, msg string) bool {
	if errStr == "invalid_grant" {
		return true
	}
	if strings.Contains(code, "ExpiredRefreshToken") {
		return true
	}
	if code == "STS5.1806" {
		// 只有「已被兑换」才是真正的终态。
		return strings.Contains(strings.ToLower(msg), "has been used")
	}
	return false
}

// normalizeExpiry 把 STS 的 expiration（ISO8601，可能带毫秒/Z）转成 RFC3339。
func normalizeExpiry(s string) string {
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05.000Z07:00",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return s // 原样保留，让上层按「未知」处理
}
