// Package traecn Trae CN 网关侧的 Go 实现。
//
// 迁移自 Trae2api-cn（Python，41935 行）：只保留 unified-api-v2 实际使用的链路
// （本地凭证解密 → IDE 通道 chat → 流式事件映射 → 工具调用桥接 → 签到/积分），
// 不引入 Responses API、Anthropic Messages、CLI 通道与企业模式。
//
// 为什么逐字节照搬常量与算法：上游 storage.json 的加密格式没有任何文档，
// 唯一的事实来源是能跑通的 Python 实现。任何"顺手优化"都可能让哈希校验失败。
package traecn

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StorageKey Trae 在 storage.json 里存认证信息的键名。
const StorageKey = "iCubeAuthInfo://icube.cloudide"

// 加密类型：由 buffer 前 6 字节判定。两种类型用不同的 salt 组合派生密钥。
const (
	encAES        = "AES"         // "tc" + 05 10 00 00
	encAESPrivate = "AES_PRIVATE" // 12 57 32 32 02 03
)

// Credentials 解密得到的认证信息。
//
// 额外保留 Raw：上游字段会随版本增加（userRegion / account / uidFusionInfo 等），
// 我们只强类型化真正要用的几个，其余留给调试与后续扩展。
type Credentials struct {
	Token            string         `json:"token"`
	RefreshToken     string         `json:"refreshToken"`
	UserID           string         `json:"userId"`
	ExpiredAt        string         `json:"expiredAt"`
	RefreshExpiredAt string         `json:"refreshExpiredAt"`
	Host             string         `json:"host"`
	Edition          string         `json:"-"` // cn / solo_cn / sg / solo_sg
	Raw              map[string]any `json:"-"`
}

// Expiry 解析 Token 过期时间；无法解析时返回零值。
func (c *Credentials) Expiry() time.Time {
	return parseTraeTime(c.ExpiredAt)
}

// RefreshExpiry 解析 refreshToken 过期时间。
func (c *Credentials) RefreshExpiry() time.Time {
	return parseTraeTime(c.RefreshExpiredAt)
}

// parseTraeTime 兼容 Trae 写入的几种时间格式（毫秒时间戳 / ISO8601）。
func parseTraeTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z0700",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// EditionDirs 返回本机各 Trae 版本的 User 目录（存在性由调用方判断）。
// 顺序即优先级：同名账号以先发现的为准，但调用方应按 token 有效性挑选。
func EditionDirs() map[string]string {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return map[string]string{}
		}
		appdata = filepath.Join(home, "AppData", "Roaming")
	}
	return map[string]string{
		"cn":      filepath.Join(appdata, "Trae CN", "User"),
		"solo_cn": filepath.Join(appdata, "TRAE SOLO CN", "User"),
		"sg":      filepath.Join(appdata, "Trae", "User"),
		"solo_sg": filepath.Join(appdata, "TRAE SOLO", "User"),
	}
}

// DecryptFromDir 从某个版本的 User 目录解出认证信息。
func DecryptFromDir(userDir string) (*Credentials, error) {
	storagePath := filepath.Join(userDir, "globalStorage", "storage.json")
	raw, err := os.ReadFile(storagePath)
	if err != nil {
		return nil, fmt.Errorf("读取 storage.json 失败: %w", err)
	}
	var storage map[string]any
	if err := json.Unmarshal(raw, &storage); err != nil {
		return nil, fmt.Errorf("storage.json 不是合法 JSON: %w", err)
	}
	v, _ := storage[StorageKey].(string)
	if v == "" {
		return nil, fmt.Errorf("storage.json 中缺少 %s（该版本可能未登录）", StorageKey)
	}
	// 国际版曾以明文 JSON 存储。
	if len(v) > 0 && v[0] == '{' {
		return decodeCredentials([]byte(v))
	}
	plain, err := DecryptStorageValue(v)
	if err != nil {
		return nil, err
	}
	return decodeCredentials([]byte(plain))
}

// decodeCredentials 把明文 JSON 解成 Credentials（同时保留 Raw）。
func decodeCredentials(plain []byte) (*Credentials, error) {
	var raw map[string]any
	if err := json.Unmarshal(plain, &raw); err != nil {
		return nil, fmt.Errorf("认证明文不是合法 JSON: %w", err)
	}
	c := &Credentials{Raw: raw}
	c.Token, _ = raw["token"].(string)
	c.RefreshToken, _ = raw["refreshToken"].(string)
	c.UserID, _ = raw["userId"].(string)
	c.ExpiredAt, _ = raw["expiredAt"].(string)
	c.RefreshExpiredAt, _ = raw["refreshExpiredAt"].(string)
	c.Host, _ = raw["host"].(string)
	if c.Token == "" {
		return nil, errors.New("认证信息里没有 token")
	}
	return c, nil
}

// AutoDiscover 依次尝试各版本目录，返回第一个 token 仍有效的凭证。
//
// 与 Python 版一致：本机可能装了多个 Trae 版本，第一个能解密的目录里 token
// 可能已过期，而另一个版本仍是登录态。这里优先返回未过期的那个；
// 全都过期时返回第一个成功解密的（供 refresh 使用）。
func AutoDiscover() (*Credentials, string, error) {
	dirs := EditionDirs()
	order := []string{"cn", "solo_cn", "sg", "solo_sg"}
	var firstOK *Credentials
	var firstEdition string
	var lastErr error
	now := time.Now()
	for _, ed := range order {
		dir, ok := dirs[ed]
		if !ok {
			continue
		}
		cred, err := DecryptFromDir(dir)
		if err != nil {
			lastErr = err
			continue
		}
		cred.Edition = ed
		if exp := cred.Expiry(); exp.IsZero() || exp.After(now) {
			return cred, ed, nil
		}
		if firstOK == nil {
			firstOK, firstEdition = cred, ed
		}
	}
	if firstOK != nil {
		return firstOK, firstEdition, nil
	}
	if lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", errors.New("未找到可用的 Trae 登录凭证")
}

// DecryptStorageValue 解密单个 tc 格式的密文值。
//
// 结构：[6B 头][32B 随机数][N 密文]
// 密文解密后：[64B SHA-512 校验][PKCS7 填充的明文 JSON]
func DecryptStorageValue(b64 string) (string, error) {
	buf, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("base64 解码失败: %w", err)
	}
	if len(buf) < 38+16 {
		return "", fmt.Errorf("密文长度异常: %d 字节", len(buf))
	}
	header := buf[:6]
	randomBytes := buf[6:38]
	encrypted := buf[38:]

	encType := detectEncType(header)
	if encType == "" {
		return "", fmt.Errorf("未知的加密类型，header=%x", header)
	}
	key, iv := deriveKeyAndIV(randomBytes, encType)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	if len(encrypted)%block.BlockSize() != 0 {
		return "", fmt.Errorf("密文长度不是块大小的整数倍: %d", len(encrypted))
	}
	decrypted := make([]byte, len(encrypted))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(decrypted, encrypted)

	if len(decrypted) < 64 {
		return "", errors.New("解密结果过短")
	}
	storedHash := decrypted[:64]
	body := pkcs7Unpad(decrypted[64:])

	sum := sha512.Sum512(body)
	if !equalBytes(storedHash, sum[:]) {
		return "", errors.New("哈希校验失败：解密结果不可信（salt 或算法与当前版本不匹配）")
	}
	return string(body), nil
}

// detectEncType 按前 6 字节判定加密类型。
func detectEncType(header []byte) string {
	if len(header) < 6 {
		return ""
	}
	if header[0] == 't' && header[1] == 'c' &&
		header[2] == 0x05 && header[3] == 0x10 && header[4] == 0x00 && header[5] == 0x00 {
		return encAES
	}
	if header[0] == 18 && header[1] == 57 && header[2] == 32 &&
		header[3] == 32 && header[4] == 2 && header[5] == 3 {
		return encAESPrivate
	}
	return ""
}

// deriveKeyAndIV 派生 AES-128-CBC 的 key 与 IV：
//
//	SHA512(random) → 拼 salt → SHA512 → 前 16B 为 key，再 16B 为 IV
func deriveKeyAndIV(randomBytes []byte, encType string) ([]byte, []byte) {
	var salt []byte
	if encType == encAESPrivate {
		salt = xorBytes(saltC, saltD)
	} else {
		salt = xorBytes(saltA, saltB)
	}
	h1 := sha512.Sum512(randomBytes)
	combined := make([]byte, 0, len(h1)+len(salt))
	combined = append(combined, h1[:]...)
	combined = append(combined, salt...)
	h2 := sha512.Sum512(combined)
	return h2[:16], h2[16:32]
}

// xorBytes 逐字节 XOR（两个输入等长，均为 64 字节）。
func xorBytes(a, b []byte) []byte {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// pkcs7Unpad 去除 PKCS7 填充；非法填充时原样返回（与 Python 版行为一致，
// 后续的哈希校验会兜住真正的问题）。
func pkcs7Unpad(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	pad := int(data[len(data)-1])
	if pad < 1 || pad > 16 || pad > len(data) {
		return data
	}
	for i := len(data) - pad; i < len(data); i++ {
		if int(data[i]) != pad {
			return data
		}
	}
	return data[:len(data)-pad]
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
