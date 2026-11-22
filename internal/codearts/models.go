package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// BuiltinModel 是 /v1/model/builtin 返回的一条模型条目（只取排别名用得上的字段）。
type BuiltinModel struct {
	ModelID        string `json:"model_id"`
	ModelName      string `json:"model_name"`
	ContextWindow  int64  `json:"context_window"`
	MaxTokens      int64  `json:"max_tokens"`
	SupportsImages bool   `json:"supports_images"`
	Enable         bool   `json:"enable"`
}

// BuiltinModels 拉取该账号可见的模型目录。
//
// 用途：别名同步——上游新增模型时，人能据此判断要不要建别名，
// 而不是靠自己去面板里逐个对照。
func (c *Client) BuiltinModels(ctx context.Context) ([]BuiltinModel, error) {
	resp, err := c.doInternal(ctx, http.MethodGet, "/v1/model/builtin", nil,
		map[string]string{"Accept": "application/json"}, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codearts: 模型目录返回 HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Models []BuiltinModel `json:"builtinModels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("codearts: 模型目录解析失败: %w", err)
	}
	return doc.Models, nil
}
