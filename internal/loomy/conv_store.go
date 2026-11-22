package loomy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ConversationStore 持久化 Loomy 的 conversationId。
//
// 为什么必须分键：conversationId 归属单个账号，换账号必须开新会话，否则
// 上游会指向一个该账号看不到的会话。所以键里必须带账号。
//
// 键形态：session | model | account
type ConversationStore struct {
	mu   sync.Mutex
	path string
	data map[string]entry

	// ttl 为 0 表示不主动过期（Loomy 侧会话本身会自然失效）。
	ttl time.Duration
}

type entry struct {
	ConversationID string `json:"conversation_id"`
	UpdatedAt      int64  `json:"updated_at"`
}

// NewConversationStore 从文件加载；文件不存在视为空表。
func NewConversationStore(path string, ttl time.Duration) *ConversationStore {
	s := &ConversationStore{path: path, data: map[string]entry{}, ttl: ttl}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(raw, &s.data)
	return s
}

func key(session, model, account string) string {
	return session + "|" + model + "|" + account
}

// Get 返回该会话当前的上游 conversationId；空串表示需要新开会话。
func (s *ConversationStore) Get(session, model, account string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[key(session, model, account)]
	if !ok {
		return ""
	}
	if s.ttl > 0 && time.Since(time.Unix(e.UpdatedAt, 0)) > s.ttl {
		delete(s.data, key(session, model, account))
		return ""
	}
	return e.ConversationID
}

// Set 记录会话 ID 并落盘。落盘失败不影响本次对话（只是下次会重开会话）。
func (s *ConversationStore) Set(session, model, account, convID string) {
	if convID == "" {
		return
	}
	s.mu.Lock()
	s.data[key(session, model, account)] = entry{ConversationID: convID, UpdatedAt: time.Now().Unix()}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err == nil {
		_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
		_ = os.WriteFile(s.path, raw, 0o600)
	}
	s.mu.Unlock()
}

// Len 返回当前条目数（面板展示用）。
func (s *ConversationStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data)
}
