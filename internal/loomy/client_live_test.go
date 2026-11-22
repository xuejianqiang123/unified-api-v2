//go:build loomy_live

// 真打上游的验证，默认不参与常规测试（需要本机已导入 loomy cookie）：
//
//	go test -tags loomy_live ./internal/loomy/ -run TestLiveUpstream -v
package loomy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveUpstream 用本机已导入的 cookie 真打一次，验证请求体与 SSE 解析。
func TestLiveUpstream(t *testing.T) {
	path := filepath.Join("..", "..", "loomy", "accounts.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("未找到 loomy 账号（%s），跳过真实上游验证", path)
	}
	var f struct {
		Accounts []struct {
			Cookie string `json:"cookie"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Accounts) == 0 {
		t.Skipf("accounts.json 为空或格式不对: %v", err)
	}
	cookie := f.Accounts[0].Cookie

	c := NewClient("")
	resp, err := c.Open(context.Background(), Request{
		MessageID: NewMessageID(),
		Model:     "GLM-5.3-Flash",
		Content:   "只回复两个字：可用",
	}, cookie)
	if err != nil {
		t.Fatalf("上游请求失败: %v", err)
	}
	defer resp.Body.Close()

	var text strings.Builder
	var conv string
	if err := ReadEvents(resp.Body, func(ev Event) error {
		if ev.ConversationID != "" {
			conv = ev.ConversationID
		}
		text.WriteString(ev.Delta)
		if text.Len() > 40 { // 拿到内容就够了，不跑完整生成
			return errEnough
		}
		return nil
	}); err != nil && err != errEnough {
		t.Fatalf("SSE 解析失败: %v", err)
	}

	t.Logf("conversation_id=%s", conv)
	t.Logf("收到文本=%q", text.String())
	if conv == "" {
		t.Error("未从响应里取到 conversation_id —— 续轮会丢掉上下文")
	}
	if text.Len() == 0 {
		t.Error("没收到任何文本增量 —— 请求体或解析有问题")
	}
}

var errEnough = &enoughErr{}

type enoughErr struct{}

func (*enoughErr) Error() string { return "enough" }
