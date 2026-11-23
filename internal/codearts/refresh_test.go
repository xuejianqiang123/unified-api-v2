package codearts

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"unified-api-v2/internal/vault"
)

// TestSaveCredentialMergesAndDedupes 同名只留 STS 最晚的一条，
// 其他账号保留 —— 这是「导入工具曾把同一次登录追加成多条」的修复面。
func TestSaveCredentialMergesAndDedupes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	old := Credential{
		Name: "acct-a", AccessKeyID: "old", SecretAccessKey: "s", SecurityToken: "t",
		ExpiresAt: "2026-11-22T14:00:00+08:00",
	}
	if err := SaveCredential(path, old); err != nil {
		t.Fatalf("首次保存: %v", err)
	}
	other := Credential{
		Name: "acct-b", AccessKeyID: "b", SecretAccessKey: "s", SecurityToken: "t",
		ExpiresAt: "2026-11-23T00:00:00+08:00",
	}
	if err := SaveCredential(path, other); err != nil {
		t.Fatalf("第二个账号: %v", err)
	}
	fresh := Credential{
		Name: "acct-a", AccessKeyID: "new", SecretAccessKey: "s", SecurityToken: "t",
		ExpiresAt: "2026-11-23T12:00:00+08:00",
	}
	if err := SaveCredential(path, fresh); err != nil {
		t.Fatalf("更新账号: %v", err)
	}

	// 经 vault 读：SaveCredential 落盘即加密，直接读文件会拿到密文。
	raw, _ := vault.ReadFile(path)
	var doc struct {
		Accounts []Credential `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Accounts) != 2 {
		t.Fatalf("账号数 = %d, want 2（acct-a 应被覆盖而非追加）", len(doc.Accounts))
	}
	for _, a := range doc.Accounts {
		if a.Name == "acct-a" && a.AccessKeyID != "new" {
			t.Errorf("acct-a 未更新为最新: %+v", a)
		}
	}
}

// TestNeedsIDERefresh 阈值判断：过期/临近过期要刷新，充裕时不打扰。
func TestNeedsIDERefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	cases := []struct {
		name      string
		expiresAt string
		want      bool
	}{
		{"已过期", time.Now().Add(-time.Hour).Format(time.RFC3339), true},
		{"剩 30 分钟", time.Now().Add(30 * time.Minute).Format(time.RFC3339), true},
		{"剩 10 小时", time.Now().Add(10 * time.Hour).Format(time.RFC3339), false},
		{"到期时间未知", "", false},
	}
	for _, c := range cases {
		if err := SaveCredential(path, Credential{
			Name: "a", AccessKeyID: "k", SecretAccessKey: "s", SecurityToken: "t", ExpiresAt: c.expiresAt,
		}); err != nil {
			t.Fatal(err)
		}
		cli := NewClient(path)
		if got := cli.NeedsIDERefresh(); got != c.want {
			t.Errorf("%s: NeedsIDERefresh = %v, want %v", c.name, got, c.want)
		}
	}
}
