//go:build windows

// Package browsercookie 从 Chromium 系浏览器（Edge / Chrome / Electron）的
// profile 中读出明文 cookie。
//
// 用途：为「本机导入」类功能提供凭证——豆包桌面客户端、专用 Edge profile 等。
// 值的加密链是 Chromium 的标准做法：
//
//	Local State 的 os_crypt.encrypted_key  --DPAPI--> AES-256 key
//	cookies.encrypted_value  --v10/v11 + AES-256-GCM--> 明文
//
// 实测补充：本机 Chromium 130+ 的 GCM 明文前还有 32 字节二进制校验头，
// 真实值从第 32 字节开始；见 stripHashPrefix 的说明。
//
// 前置条件：目标浏览器的该 profile 必须已退出。Windows 对 SQLite 文件加独占锁，
// 进程运行期间打不开（实测 sqlite3 immutable/readonly、复制文件均失败）。
package browsercookie

import (
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	_ "modernc.org/sqlite"
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	modCrypt32    = syscall.NewLazyDLL("crypt32.dll")
	procUnprotect = modCrypt32.NewProc("CryptUnprotectData")
	modKernel32   = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree = modKernel32.NewProc("LocalFree")
)

// Read 读取 userDataDir 下某个 profile 里 hostSuffix 域的 cookie。
//
// userDataDir 是浏览器 User Data 根目录（含 Local State）；
// profile 是 "Default" / "Profile 1" 之类；hostSuffix 传 "xunfei.cn" 会匹配
// .xunfei.cn / loomy.xunfei.cn。
func Read(userDataDir, profile, hostSuffix string) (map[string]string, error) {
	key, err := loadAESKey(userDataDir)
	if err != nil {
		return nil, err
	}
	cookiesPath := filepath.Join(userDataDir, profile, "Network", "Cookies")
	if _, err := os.Stat(cookiesPath); err != nil {
		return nil, fmt.Errorf("未找到 Cookies 库（%s）：该 profile 可能还没打开过或路径不对", cookiesPath)
	}
	return readValues(cookiesPath, key, hostSuffix)
}

// loadAESKey 解出 Local State 里 DPAPI 包裹的 AES key。
func loadAESKey(userDataDir string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(userDataDir, "Local State"))
	if err != nil {
		return nil, fmt.Errorf("读取 Local State 失败（该 profile 是否启动过？）: %w", err)
	}
	var st struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("Local State 不是合法 JSON: %w", err)
	}
	if st.OSCrypt.EncryptedKey == "" {
		return nil, errors.New("Local State 中没有 os_crypt.encrypted_key")
	}
	key, err := base64.StdEncoding.DecodeString(st.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("encrypted_key base64 解码失败: %w", err)
	}
	if len(key) < 5 || string(key[:5]) != "DPAPI" {
		return nil, errors.New("加密密钥不是 DPAPI 格式（可能与当前 Windows 用户不匹配）")
	}
	return dpapiUnprotect(key[5:])
}

// dpapiUnprotect 调用 CryptUnprotectData，必须在加密时的同一 Windows 用户下运行。
func dpapiUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("DPAPI 输入为空")
	}
	var in dataBlob
	in.cbData = uint32(len(blob))
	in.pbData = &blob[0]
	var out dataBlob
	ret, _, err := procUnprotect.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptUnprotectData 失败（是否换了 Windows 用户运行？）: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	if out.pbData == nil || out.cbData == 0 {
		return nil, errors.New("DPAPI 返回空数据")
	}
	src := unsafe.Slice(out.pbData, out.cbData)
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst, nil
}

// decryptValue 解一条 encrypted_value：v10/v11 = 前缀(3) + nonce(12) + 密文 + tag(16)。
func decryptValue(enc, key []byte) (string, error) {
	if len(enc) < 3 {
		return "", errors.New("密文过短")
	}
	switch string(enc[:3]) {
	case "v10", "v11":
		payload := enc[3:]
		if len(payload) < 12+16 {
			return "", errors.New("密文长度不足（缺 nonce 或 tag）")
		}
		if len(key) != 32 {
			return "", fmt.Errorf("AES key 长度应为 32，实际 %d", len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return "", err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return "", err
		}
		plain, err := gcm.Open(nil, payload[:12], payload[12:], nil)
		if err != nil {
			return "", fmt.Errorf("GCM 解密失败（key 与数据不匹配）: %w", err)
		}
		return string(plain), nil
	case "v20":
		return "", errors.New("该 cookie 使用 app-bound 加密（v20），暂不支持")
	default:
		return string(enc), nil // 早期版本可能直接存明文
	}
}

func allPrintableASCII(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// stripHashPrefix 去掉明文前的 32 字节完整性头。
//
// 判据保守：头含非可打印字符 且 后段全可打印，才认为是头、剥掉；
// 旧格式（整段纯 ASCII）不会被误切。宁可少剥也不能把值切断。
func stripHashPrefix(v []byte) []byte {
	if len(v) <= 32 {
		return v
	}
	if !allPrintableASCII(v[:32]) && allPrintableASCII(v[32:]) {
		return v[32:]
	}
	return v
}

func readValues(cookiesPath string, key []byte, hostSuffix string) (map[string]string, error) {
	db, err := openCookieDB(cookiesPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(
		`select host_key, name, value, encrypted_value from cookies where host_key like ?`,
		"%"+hostSuffix+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("查询 cookies 表失败: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var host, name, plain string
		var encrypted []byte
		if err := rows.Scan(&host, &name, &plain, &encrypted); err != nil {
			return nil, err
		}
		if name == "" {
			continue
		}
		if plain != "" {
			out[name] = plain
			continue
		}
		if len(encrypted) == 0 {
			continue
		}
		val, derr := decryptValue(encrypted, key)
		if derr != nil {
			continue // 单条解不出不影响整体
		}
		raw := stripHashPrefix([]byte(val))
		if !allPrintableASCII(raw) {
			continue // 剥完仍不可打印，说明格式未知，宁可不导入也不写垃圾
		}
		out[name] = string(raw)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// openCookieDB 以只读方式打开 Cookies 库，并把「被进程独占」翻译成可操作的中文。
func openCookieDB(path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(2000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, translateLockErr(err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, translateLockErr(err)
	}
	return db, nil
}

func translateLockErr(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	for _, hint := range []string{"unable to open database file", "database is locked", "sharing violation", "access is denied"} {
		if strings.Contains(msg, hint) {
			return errors.New("目标浏览器正在运行并独占 Cookies 文件，请完全关闭该浏览器窗口后重试")
		}
	}
	return err
}

// JoinHeader 把 name→value 拼成请求头格式；prefer 里的键排前面，其余按字典序。
func JoinHeader(m map[string]string, prefer []string) string {
	seen := map[string]bool{}
	parts := make([]string, 0, len(m))
	for _, k := range prefer {
		if v, ok := m[k]; ok && !seen[k] {
			parts = append(parts, k+"="+v)
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, "; ")
}
