// Package vault 给落盘凭据做静态加密（AES-256-GCM）。
//
// 为什么要有它：账号文件里是 AK/SK、refresh_token、浏览器 Cookie —— 明文落盘
// 意味着「拿到文件 = 拿到账号」。而这类文件恰好又经常被复制进备份、上传到网盘。
//
// 设计取向（保守）：
//   - **只对显式调用本包的文件加密**，不碰 config.json 这类需要人工编辑的文件；
//   - 明文文件仍可读（首字符不是 magic），所以升级不需要停机迁移；
//   - 首次写入自动转成密文，之后每次覆盖都是密文。
package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// magic 是密文文件的标识。6 字节，足以和 JSON（'{' / '['）区分。
var magic = []byte("UVLT1\x00")

// DefaultKeyPath 密钥文件位置（与 data/ 同级，权限 0600）。
const DefaultKeyPath = "data/.vaultkey"

// keyCache 缓存已加载的密钥，避免每次读写都读盘。
var keyCache []byte

// Key 返回 32 字节主密钥；不存在则生成一个。
//
// ⚠️ 密钥与数据必须一起备份：密钥丢了，密文就再也解不开。
func Key() ([]byte, error) {
	if keyCache != nil {
		return keyCache, nil
	}
	if raw, err := os.ReadFile(DefaultKeyPath); err == nil && len(raw) == 32 {
		keyCache = raw
		return keyCache, nil
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("vault: 生成密钥失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(DefaultKeyPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(DefaultKeyPath, k, 0o600); err != nil {
		return nil, fmt.Errorf("vault: 写入密钥失败: %w", err)
	}
	keyCache = k
	return k, nil
}

// IsEncrypted 判断一段内容是密文还是明文 JSON。
func IsEncrypted(raw []byte) bool {
	return bytes.HasPrefix(raw, magic)
}

// Encrypt 加密明文，返回 magic|nonce|ciphertext。
func Encrypt(plain []byte) ([]byte, error) {
	k, err := Key()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plain, magic)
	out := make([]byte, 0, len(magic)+len(nonce)+len(ct))
	out = append(out, magic...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// Decrypt 解密密文；传入明文则原样返回（方便「还没迁移」的文件）。
func Decrypt(raw []byte) ([]byte, error) {
	if !IsEncrypted(raw) {
		return raw, nil
	}
	k, err := Key()
	if err != nil {
		return nil, err
	}
	body := raw[len(magic):]
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(body) < gcm.NonceSize() {
		return nil, fmt.Errorf("vault: 密文长度异常")
	}
	nonce, ct := body[:gcm.NonceSize()], body[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, magic)
	if err != nil {
		// 最常见原因：换过密钥、或文件来自另一台机器。
		return nil, fmt.Errorf("vault: 解密失败（密钥不匹配或文件已损坏）: %w", err)
	}
	return plain, nil
}

// ReadFile 读文件并自动解密（明文原样返回）。
func ReadFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decrypt(raw)
}

// WriteFile 加密后落盘（权限 0600）。
func WriteFile(path string, plain []byte) error {
	enc, err := Encrypt(plain)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, enc, 0o600)
}
