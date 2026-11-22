package codearts

import (
	"encoding/json"
	"testing"
)

// TestParseCredentialFilePicksLatestSTS 锁住一条真实踩过的坑：
// 导入工具曾因 name 没写回而把同一次登录追加成两条，旧的在前、新的在后。
// 若「取第一个」，网关会一直用到快过期的那份，表现为「刚导入就报凭据过期」。
func TestParseCredentialFilePicksLatestSTS(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"accounts": []map[string]any{
		{"access_key_id": "old", "secret_access_key": "s1", "security_token": "t1",
			"expires_at": "2026-11-22T14:29:12+08:00"},
		{"access_key_id": "new", "secret_access_key": "s2", "security_token": "t2",
			"expires_at": "2026-11-22T16:24:14+08:00"},
		// 没有 STS 的条目应被跳过（半截凭据不能当可用账号）。
		{"name": "broken"},
	}})
	cred, err := ParseCredentialFile(raw)
	if err != nil {
		t.Fatalf("ParseCredentialFile: %v", err)
	}
	if cred.AccessKeyID != "new" {
		t.Errorf("应选 STS 到期最晚的账号，实际选了 %q", cred.AccessKeyID)
	}
}

// TestParseCredentialFileSingleObject 单对象形态（旧文件）仍可用。
func TestParseCredentialFileSingleObject(t *testing.T) {
	raw, _ := json.Marshal(Credential{AccessKeyID: "a", SecretAccessKey: "b", SecurityToken: "c"})
	cred, err := ParseCredentialFile(raw)
	if err != nil || cred.AccessKeyID != "a" {
		t.Fatalf("单对象解析失败: cred=%+v err=%v", cred, err)
	}
}
