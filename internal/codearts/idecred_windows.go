//go:build windows

package codearts

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
	"unsafe"
)

// IDEUserDataDir 返回码道 IDE 的 userData 目录（凭据与 Local State 所在）。
func IDEUserDataDir() string {
	if v := os.Getenv("CODEARTS_USER_DATA"); v != "" {
		return v
	}
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return ""
	}
	return filepath.Join(appData, "codearts")
}

// ErrIDENotFound 表示本机没有找到已登录的码道凭据。
var ErrIDENotFound = errors.New("codearts: 本机没有找到已登录的码道凭据（请先在 IDE 里登录一次）")

// ImportFromIDE 从本机已登录的码道 IDE 读出凭据。
//
// 只**读**，绝不写 IDE 的任何文件：IDE 自己负责续期并把新 STS 写回，
// 网关读同一份来源即可跟上。这样不会碰那个一次性轮换的 refresh_token ——
// 一旦网关自己拿它去换票，IDE 的登录就废了。
func ImportFromIDE() (Credential, error) {
	dir := IDEUserDataDir()
	if dir == "" {
		return Credential{}, ErrIDENotFound
	}
	key, err := oscryptMasterKey(dir)
	if err != nil {
		return Credential{}, err
	}
	encFiles, err := filepath.Glob(filepath.Join(dir, "secretStorage", "*.enc"))
	if err != nil || len(encFiles) == 0 {
		return Credential{}, ErrIDENotFound
	}
	// 多个存储文件时取**最新修改**的：登录/续期都会重写同一个。
	sort.Slice(encFiles, func(i, j int) bool {
		fi, e1 := os.Stat(encFiles[i])
		fj, e2 := os.Stat(encFiles[j])
		if e1 != nil || e2 != nil {
			return false
		}
		return fi.ModTime().After(fj.ModTime())
	})
	var lastErr error
	for _, f := range encFiles {
		plain, err := decryptOSCryptFile(f, key)
		if err != nil {
			lastErr = err
			continue
		}
		cred, err := parseIDECredential(plain)
		if err != nil {
			lastErr = err
			continue
		}
		return cred, nil
	}
	if lastErr == nil {
		lastErr = ErrIDENotFound
	}
	return Credential{}, lastErr
}

// oscryptMasterKey 从 Local State 取 Chromium OSCrypt 主密钥（DPAPI 保护）。
func oscryptMasterKey(dir string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "Local State"))
	if err != nil {
		return nil, fmt.Errorf("codearts: 读 Local State 失败（IDE 未登录过？）: %w", err)
	}
	var ls struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("codearts: 解析 Local State 失败: %w", err)
	}
	enc, err := base64.StdEncoding.DecodeString(ls.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("codearts: encrypted_key 不是合法 base64: %w", err)
	}
	if len(enc) < 5 || string(enc[:5]) != "DPAPI" {
		return nil, errors.New("codearts: Local State 的主密钥不是 DPAPI 形态")
	}
	return dpapiUnprotect(enc[5:])
}

// decryptOSCryptFile 解 "v10" + nonce(12) + ciphertext + tag(16) 形态的密文。
func decryptOSCryptFile(path string, key []byte) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blob, err := base64.StdEncoding.DecodeString(string(trimSpaceBytes(raw)))
	if err != nil {
		return nil, fmt.Errorf("codearts: %s 不是合法 base64: %w", filepath.Base(path), err)
	}
	if len(blob) < 3+12+16 || string(blob[:3]) != "v10" {
		return nil, fmt.Errorf("codearts: %s 不是 OSCrypt v10 形态", filepath.Base(path))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := blob[3:15]
	return gcm.Open(nil, nonce, blob[15:], nil)
}

// ideSecret 是 secretStorage 明文的结构（只取我们用得到的字段）。
type ideSecret struct {
	Session struct {
		AccessKey     string `json:"accessKey"`
		SecretKey     string `json:"secretKey"`
		SecurityToken string `json:"securityToken"`
		RefreshToken  string `json:"refreshToken"`
		ExpiresAt     int64  `json:"expiresAt"` // 毫秒时间戳
		UserName      string `json:"userName"`
	} `json:"session"`
	LoginContext struct {
		PkcePair struct {
			CodeVerifier string `json:"codeVerifier"`
		} `json:"pkcePair"`
		DpopKeyPair struct {
			PrivateKey string `json:"privateKey"` // JWK JSON 字符串
		} `json:"dpopKeyPair"`
	} `json:"loginContext"`
}

func parseIDECredential(plain []byte) (Credential, error) {
	var s ideSecret
	if err := json.Unmarshal(plain, &s); err != nil {
		return Credential{}, fmt.Errorf("codearts: 凭据明文不是预期结构: %w", err)
	}
	cred := Credential{
		AccessKeyID:     s.Session.AccessKey,
		SecretAccessKey: s.Session.SecretKey,
		SecurityToken:   s.Session.SecurityToken,
		RefreshToken:    s.Session.RefreshToken,
		CodeVerifier:    s.LoginContext.PkcePair.CodeVerifier,
		UserName:        s.Session.UserName,
	}
	if s.Session.ExpiresAt > 0 {
		cred.ExpiresAt = millisToRFC3339(s.Session.ExpiresAt)
	}
	if jwk := s.LoginContext.DpopKeyPair.PrivateKey; jwk != "" {
		var k DpopJWK
		if err := json.Unmarshal([]byte(jwk), &k); err == nil {
			cred.DpopPrivateJWK = &k
		}
	}
	if !cred.HasSTS() {
		return Credential{}, errors.New("codearts: IDE 凭据里缺少 AK/SK/security_token")
	}
	return cred, nil
}

// --- DPAPI ---

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func dpapiUnprotect(in []byte) ([]byte, error) {
	if len(in) == 0 {
		return nil, errors.New("codearts: DPAPI 输入为空")
	}
	var inBlob, outBlob dataBlob
	inBlob.cbData = uint32(len(in))
	inBlob.pbData = &in[0]

	crypt32 := syscall.NewLazyDLL("crypt32.dll")
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procUnprotect := crypt32.NewProc("CryptUnprotectData")
	procLocalFree := kernel32.NewProc("LocalFree")

	const cryptprotectUIForbidden = 0x1
	r, _, err := procUnprotect.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, 0, 0, 0,
		cryptprotectUIForbidden,
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if r == 0 {
		return nil, fmt.Errorf("codearts: DPAPI 解密失败（是否换了 Windows 用户？）: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.pbData)))
	out := make([]byte, outBlob.cbData)
	copy(out, unsafe.Slice(outBlob.pbData, outBlob.cbData))
	return out, nil
}

func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpaceByte(b[start]) {
		start++
	}
	for end > start && isSpaceByte(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t'
}

func millisToRFC3339(ms int64) string {
	return time.UnixMilli(ms).Format("2006-01-02T15:04:05Z07:00")
}
