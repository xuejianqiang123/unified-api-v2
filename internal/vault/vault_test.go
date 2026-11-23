package vault

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRoundTrip 加密后必须能原样读回，且落盘内容不含明文。
func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "acc.json")
	plain := []byte(`{"name":"x","refresh_token":"secret-value"}`)

	if err := WriteFile(path, plain); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !IsEncrypted(raw) {
		t.Fatal("落盘内容应为密文")
	}
	if string(raw) == string(plain) {
		t.Fatal("落盘内容与明文相同")
	}
	if contains(raw, []byte("secret-value")) {
		t.Fatal("密文里出现了明文片段（说明没真加密）")
	}

	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("往返不一致: %s", got)
	}
}

// TestPlaintextCompatible 明文文件必须能读（升级不需要迁移）。
func TestPlaintextCompatible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.json")
	plain := []byte(`{"name":"legacy"}`)
	if err := os.WriteFile(path, plain, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("明文应可直接读: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatal("明文读取被改动")
	}
}

// TestWrongKeyFails 密钥不匹配必须报错，不能返回垃圾数据。
func TestWrongKeyFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	if err := WriteFile(path, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	// 伪造一个新密钥
	keyCache = make([]byte, 32)
	if _, err := ReadFile(path); err == nil {
		t.Fatal("错误密钥应解密失败")
	}
	keyCache = nil
}

func contains(hay, needle []byte) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if string(hay[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
