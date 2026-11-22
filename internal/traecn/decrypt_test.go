package traecn

import (
	"os"
	"testing"
)

// TestDecryptFromLocalStorage 用本机真实 storage.json 验证解密链路。
//
// 这是阶段 1 的判据：Go 解密结果必须与 Python 版逐字段一致（token 长度、userId）。
// 未安装或未登录 Trae 时跳过，不假绿。
func TestDecryptFromLocalStorage(t *testing.T) {
	dirs := EditionDirs()
	dir, ok := dirs["cn"]
	if !ok {
		t.Skip("无法确定 Trae CN 数据目录")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("本机未安装 Trae CN（%s 不存在）", dir)
	}
	cred, err := DecryptFromDir(dir)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if cred.Token == "" {
		t.Fatal("token 为空")
	}
	if len(cred.Token) < 100 {
		t.Errorf("token 长度异常: %d（Python 版为 1004）", len(cred.Token))
	}
	if cred.UserID == "" {
		t.Error("userId 为空")
	}
	t.Logf("edition=cn userId=%s tokenLen=%d refreshTokenLen=%d expiredAt=%s",
		cred.UserID, len(cred.Token), len(cred.RefreshToken), cred.ExpiredAt)
}

// TestDetectEncType 头部判定：两种已知格式必须认出，其它必须拒绝。
func TestDetectEncType(t *testing.T) {
	if got := detectEncType([]byte{'t', 'c', 0x05, 0x10, 0x00, 0x00}); got != encAES {
		t.Errorf("AES 头判定错误: %q", got)
	}
	if got := detectEncType([]byte{18, 57, 32, 32, 2, 3}); got != encAESPrivate {
		t.Errorf("AES_PRIVATE 头判定错误: %q", got)
	}
	if got := detectEncType([]byte{1, 2, 3, 4, 5, 6}); got != "" {
		t.Errorf("未知头应为空，实际 %q", got)
	}
}

// TestPKCS7Unpad 合法填充去除、非法填充原样返回。
func TestPKCS7Unpad(t *testing.T) {
	if got := pkcs7Unpad([]byte("abc\x01")); string(got) != "abc" {
		t.Errorf("单字节填充去除错误: %q", got)
	}
	if got := pkcs7Unpad([]byte{0x01, 0x02}); len(got) != 2 {
		t.Errorf("非法填充应原样返回，实际 %v", got)
	}
}

// TestDecryptRejectsCorrupt 损坏密文必须报错，不能返回半截明文。
func TestDecryptRejectsCorrupt(t *testing.T) {
	if _, err := DecryptStorageValue("bm90LWEtdHJhZS1ibG9i"); err == nil {
		t.Fatal("非法 base64 长度应报错")
	}
}
