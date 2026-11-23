package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
	"unified-api-v2/internal/vault"
)

// RefreshThreshold 是「该去 IDE 重读凭据」的剩余时间阈值。
//
// 取 2 小时：STS 有效期约 24 小时，留出足够余量让一次失败还能在下次重试；
// 又不至于频繁读 IDE 的 secretStorage（那是跨应用访问，越少越好）。
const RefreshThreshold = 2 * time.Hour

// NeedsIDERefresh 判断是否需要从 IDE 重新导入凭据。
//
// 三种情况返回 true：
//  1. 完全读不到凭据（文件缺失/损坏）；
//  2. STS 已过期；
//  3. 剩余时间已低于 RefreshThreshold。
//
// 注意：这是**建议性**判断，调用方仍应容忍刷新失败并保留旧凭据 ——
// IDE 可能没开、可能换了 Windows 用户，那时不该把还能用的旧凭据丢掉。
func (c *Client) NeedsIDERefresh() bool {
	if c == nil || c.credPath == "" {
		return false
	}
	cred, err := c.Credential()
	if err != nil {
		return true
	}
	if cred.ExpiresAt == "" {
		return false // 不知道到期时间：不主动打扰
	}
	t, perr := time.Parse(time.RFC3339, cred.ExpiresAt)
	if perr != nil {
		return false
	}
	return time.Until(t) < RefreshThreshold
}

// RefreshFromIDE 从本机 IDE 重新读凭据并写回账号文件。
//
// 只读 IDE、不写 IDE：IDE 自己负责续期，这里只是把它已经续好的那份抄过来。
// 这是「不碰 refresh_token」策略的延伸 —— 网关始终不持有续期权。
func (c *Client) RefreshFromIDE() error {
	if c == nil || c.credPath == "" {
		return fmt.Errorf("codearts: 未配置凭据路径")
	}
	cred, err := ImportFromIDE()
	if err != nil {
		return err
	}
	if err := SaveCredential(c.credPath, cred); err != nil {
		return err
	}
	// 让下一次 Credential() 立刻读到新值，而不是等文件轮询。
	c.mu.Lock()
	c.last = cred
	c.mu.Unlock()
	return nil
}

// SaveCredential 把一份凭据合并进账号文件：同名覆盖、按 STS 到期去重、保留其他账号。
//
// 为什么需要「按到期时间取最新」：历史版本曾因 name 未写回而把同一次登录追加成
// 多条，若按「第一个」读取会一直用到快过期那份（表现为「刚导入就报过期」）。
func SaveCredential(path string, cred Credential) error {
	if cred.Name == "" {
		cred.Name = credentialName(cred)
	}
	if cred.Name == "" {
		cred.Name = "codearts"
	}

	var accounts []Credential
	if raw, err := vault.ReadFile(path); err == nil {
		var doc struct {
			Accounts []Credential `json:"accounts"`
		}
		if json.Unmarshal(raw, &doc) == nil {
			accounts = doc.Accounts
		}
	}

	replaced := false
	for i := range accounts {
		if credentialName(accounts[i]) == cred.Name {
			accounts[i] = cred
			replaced = true
			break
		}
	}
	if !replaced {
		accounts = append(accounts, cred)
	}

	// 同名归并：只保留 STS 到期最晚的一条（RFC3339 可直接字典序比较）。
	best := map[string]Credential{}
	order := make([]string, 0, len(accounts))
	for _, a := range accounts {
		key := credentialName(a)
		if key == "" {
			key = "codearts"
		}
		cur, ok := best[key]
		if !ok {
			best[key] = a
			order = append(order, key)
			continue
		}
		if a.ExpiresAt > cur.ExpiresAt {
			best[key] = a
		}
	}
	out := make([]Credential, 0, len(order))
	for _, key := range order {
		out = append(out, best[key])
	}

	raw, err := json.MarshalIndent(struct {
		Accounts []Credential `json:"accounts"`
	}{out}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// 0600：凭据等价于账号密码，同机其他用户不该能读。
	// 写盘即加密：此后该文件是密文，密钥在 data/.vaultkey。
	if err := vault.WriteFile(path, raw); err != nil {
		return err
	}
	// 覆盖写不会改权限（旧文件可能是 0644），显式收紧一次。
	if err := os.Chmod(path, 0o600); err != nil {
		log.Printf("codearts: 收紧凭据文件权限失败（不影响使用）: %v", err)
	}
	return nil
}

// credentialName 取账号显示名：优先 Name，空则从 refresh_token 解。
func credentialName(c Credential) string {
	if c.Name != "" {
		return c.Name
	}
	if n := IdentityFromCredential(c).Name; n != "" {
		return n
	}
	return c.UserName
}

// ecdsaGenerate 抽出生成逻辑，供登录时生成新 DPoP 密钥。
func ecdsaGenerate() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}
