//go:build windows

// chromecookie_windows.go 从 Chromium/Electron 的 cookie 库中读出明文 cookie。
//
// 用于「导入本机豆包」：豆包桌面客户端是 Electron，登录态存在
// User Data/Default/Network/Cookies（SQLite），值用 AES-256-GCM 加密，
// key 由 Local State 的 os_crypt.encrypted_key 经 Windows DPAPI 包裹。
//
// 前置条件：豆包客户端必须完全退出。Windows 会对该 SQLite 文件加独占锁，
// 客户端运行期间任何方式都打不开（实测 sqlite3 immutable/readonly 均失败）。
package dbserver

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

// dataBlob 对应 Win32 的 DATA_BLOB。
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

// dpapiUnprotect 调用 CryptUnprotectData 在当前用户上下文解密。
// 必须与加密时同一 Windows 用户，否则会失败（DPAPI 的用户级密钥）。
func dpapiUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("DPAPI 输入为空")
	}
	var in dataBlob
	in.cbData = uint32(len(blob))
	in.pbData = &blob[0]
	var out dataBlob
	ret, _, err := procUnprotect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // ppszDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		0, // dwFlags
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

// loadChromiumAESKey 读取 Local State 里的 os_crypt.encrypted_key 并解出 32 字节 key。
func loadChromiumAESKey(userDataDir string) ([]byte, error) {
	path := filepath.Join(userDataDir, "Local State")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 Local State 失败: %w", err)
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
		return nil, errors.New("加密密钥不是 DPAPI 格式，可能来自其它平台的配置")
	}
	return dpapiUnprotect(key[5:])
}

// decryptChromiumValue 解一条 encrypted_value。
//
// v10/v11：3 字节前缀 + 12 字节 nonce + 密文 + 16 字节 tag（AES-256-GCM）。
// v20：Chrome 127+ 的 app-bound 加密，需要额外校验，这里明确不支持而不是瞎猜。
func decryptChromiumValue(enc, key []byte) (string, error) {
	if len(enc) < 3 {
		return "", errors.New("密文过短")
	}
	prefix := string(enc[:3])
	switch prefix {
	case "v10", "v11":
		payload := enc[3:]
		if len(payload) < 12+16 {
			return "", errors.New("密文长度不足（缺少 nonce 或 tag）")
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
		nonce, ciphertext := payload[:12], payload[12:]
		plain, err := gcm.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			return "", fmt.Errorf("GCM 解密失败（key 与数据不匹配）: %w", err)
		}
		return string(plain), nil
	case "v20":
		return "", errors.New("该 cookie 使用 app-bound 加密（v20），暂不支持；请改用扫码或粘贴 Cookie")
	default:
		// 早期 Chromium 可能直接存明文。
		return string(enc), nil
	}
}

// allPrintableASCII 判断字节串是否全为可打印 ASCII（cookie 值的合法字符集）。
func allPrintableASCII(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// stripCookieHashPrefix 去掉明文前的 32 字节完整性头。
//
// 实测本机豆包客户端（Chromium 130+ 内核）的 GCM 明文布局是
// [32B 二进制校验头][真实 cookie 值]：sessionid 剥掉后正好是 32 位 hex。
// 判据用「头含非可打印字符 且 后段全可打印」，这样旧格式（整段纯 ASCII）
// 不会被误剥——宁可少剥也不能把值切断。
func stripCookieHashPrefix(v []byte) []byte {
	if len(v) <= 32 {
		return v
	}
	head, tail := v[:32], v[32:]
	if !allPrintableASCII(head) && allPrintableASCII(tail) {
		return tail
	}
	return v
}

// chromiumCookieValues 读 cookies 表并解出 name → value。
func chromiumCookieValues(cookiesPath string, key []byte, hostSuffix string) (map[string]string, error) {
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
		// value 与 encrypted_value 互斥：新版只填后者。
		if plain != "" {
			out[name] = plain
			continue
		}
		if len(encrypted) == 0 {
			continue
		}
		val, derr := decryptChromiumValue(encrypted, key)
		if derr != nil {
			// 单条解不出不影响整体：跳过并保留其它可用 cookie。
			continue
		}
		raw := stripCookieHashPrefix([]byte(val))
		if !allPrintableASCII(raw) {
			// 剥完仍不可打印，说明是未知格式，宁可不导入也不要写垃圾进号池。
			continue
		}
		out[name] = string(raw)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// joinCookieHeader 把 name→value 拼成请求头格式（按 key 排序保证结果稳定）。
func joinCookieHeader(m map[string]string, prefer []string) string {
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

// openCookieDB 以只读方式打开 Chromium 的 Cookies 库。
//
// Windows 上客户端运行时会独占该文件，此时所有打开方式（含 immutable=1）都会
// 报 "unable to open database file"。这里把这类错误翻译成可操作的中文提示，
// 避免用户在面板上只看到一句英文报错。
func openCookieDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("未找到 Cookies 库（%s）: %w", path, err)
	}
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(2000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, translateCookieLockErr(err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, translateCookieLockErr(err)
	}
	return db, nil
}

// translateCookieLockErr 识别「文件被客户端独占」这一类错误并给出处置建议。
func translateCookieLockErr(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	for _, hint := range []string{"unable to open database file", "database is locked", "sharing violation", "access is denied"} {
		if strings.Contains(msg, hint) {
			return errors.New("目标客户端正在运行并独占 Cookies 文件，请完全退出该客户端（含托盘图标）后重试")
		}
	}
	return err
}
