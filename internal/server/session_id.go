package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// HeaderSessionID 统一网关转发给豆包 provider 的稳定会话标识头。
//
// 为什么需要它：豆包侧按 client_meta.conversation_id 复用会话。没有稳定标识时，
// dbserver 只能按「首条 user 消息哈希」定会话键，而 OpenAI 兼容客户端通常每轮
// 只发最新一条消息——键每轮都变，于是每轮都新建一个豆包会话，豆包界面表现为
// 一堆互不相干的对话。用调用方身份派生标识，才能让同一调用方的多轮请求落进同
// 一个豆包会话。
const HeaderSessionID = "X-Session-Id"

// doubaoSessionID 返回本次转发应携带的稳定会话标识。
//
// 优先级：
//  1. 请求显式携带的 X-Session-Id（面板测试台等内部调用自行指定，原样透传）；
//  2. 鉴权密钥派生（同一调用方密钥 → 同一标识，不含明文密钥）；
//  3. 不鉴权部署回落固定标识（单用户场景等价于「一个客户端一个会话」）。
//
// 刻意不用消息内容派生：内容随轮次变化，正是会话被打碎的原因。
func doubaoSessionID(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(HeaderSessionID)); v != "" {
		return v
	}
	if k := authKeyFrom(r.Context()); k != nil && k.Key != "" {
		sum := sha256.Sum256([]byte(k.Key))
		return "client-" + hex.EncodeToString(sum[:8])
	}
	return "default"
}
