package codearts

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Identity 是凭据里能解出的账号身份。
//
// CodeArts 的 access_key_id 是**临时 AK**，每次续期都换，不能当稳定标识；
// 真正跨续期稳定的是 user_profile.account_id。
type Identity struct {
	// Name 是展示用名字（IAM 用户名）。子用户是「子用户名@账号名」，root 是裸名。
	// 解不出时为空串 —— 调用方回落池 id，绝不编造。
	Name string
	// AccountID 是 IAM 账号 id，稳定身份。
	AccountID string
}

// IdentityFromCredential 从凭据解出展示名与账号 id（纯函数、零 IO、不打印 token）。
//
// 名字的唯一来源是 refresh_token 这个 JWT 的 user_profile 字段：华为 STS 签发的
// 它里嵌了一段 base64 JSON。实测没有任何在线端点能取到用户名
// （snap-manager/statistics/plugin 零用户名字段，/user/info 系列全 404，
// IAM 用户列表用临时 AK 调是 403）⇒ 解 JWT 不是「更省」，是唯一可行路径。
func IdentityFromCredential(c Credential) Identity {
	if c.RefreshToken != "" {
		if profile, ok := readUserProfile(c.RefreshToken); ok {
			accountName := readStr(profile, "account_name")
			urnName := readUrnUserName(profile)
			accountID := readStr(profile, "account_id")
			isRoot, _ := profile["principal_is_root_user"].(bool)

			var name string
			switch {
			case urnName == "":
				name = accountName
			case accountName == "" || accountName == urnName:
				name = urnName
			case isRoot:
				// root 账号的名字唯一，没有子用户需要消歧 ⇒ 裸名。
				// 拼成 a@b 会渲染出一个不存在的账号名。
				name = accountName
			default:
				name = urnName + "@" + accountName
			}
			return Identity{Name: name, AccountID: accountID}
		}
	}
	// 旧式 ticket 凭据带 user_name；新式 OAuth 凭据没有。
	return Identity{Name: strings.TrimSpace(c.UserName)}
}

// readUserProfile 解 JWT 中段 → payload.user_profile（内嵌 base64 JSON）。
//
// 不解签名：只用于展示名，不作任何信任判断；载荷来自本地凭据文件。
func readUserProfile(refreshToken string) (map[string]any, bool) {
	parts := strings.Split(refreshToken, ".")
	if len(parts) != 3 || parts[1] == "" {
		return nil, false
	}
	payloadRaw, err := decodeBase64URL(parts[1])
	if err != nil {
		return nil, false
	}
	var payload map[string]any
	if json.Unmarshal(payloadRaw, &payload) != nil {
		return nil, false
	}
	inner, ok := payload["user_profile"].(string)
	if !ok || inner == "" {
		return nil, false
	}
	profileRaw, err := decodeBase64URL(inner)
	if err != nil {
		return nil, false
	}
	var profile map[string]any
	if json.Unmarshal(profileRaw, &profile) != nil {
		return nil, false
	}
	return profile, true
}

// decodeBase64URL 必须同时吃带填充与不带填充两种形状。
//
// 实测不统一：同一端点签发的 9 个账号里 6 个带 '='、3 个不带；另一台机器 4 个
// 全不带。自己写 atob + 手工补 '=' 补错会**静默解出乱码 JSON**（不报错，只是名字没了）。
func decodeBase64URL(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	// 兜底：上游若用标准字母表（+ /）而非 URL 安全（- _）。
	return base64.RawStdEncoding.DecodeString(s)
}

// readUrnUserName 取 principal_urn 末段（iam::<accountId>:user:<name>）。
//
// 用最后一个 ":user:" 之后的部分，而不是 split(':') 取末元素：
// 后者在用户名含冒号时会错位，且末段为空时会静默得到空串。
func readUrnUserName(profile map[string]any) string {
	urn := readStr(profile, "principal_urn")
	at := strings.LastIndex(urn, ":user:")
	if at < 0 {
		return ""
	}
	return strings.TrimSpace(urn[at+len(":user:"):])
}

func readStr(m map[string]any, key string) string {
	v, ok := m[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
