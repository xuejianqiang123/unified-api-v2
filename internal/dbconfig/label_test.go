package dbconfig

import "testing"

// TestModelLabelResolvesInternalKey 守卫面板「实际所用模型」的可读性。
//
// 背景：豆包的 model_item_key 有数字形态（auto → "9"），面板曾直接把 "9" 当模型名
// 展示，被运维判为「日志没有有效信息」。ModelLabel 负责反向解析回人类可读名。
func TestModelLabelResolvesInternalKey(t *testing.T) {
	c := &Config{
		Models: []string{"auto", "doubao-2.1-lite", "deepseek-v4-flash"},
		ModelAliases: map[string]string{
			"auto":              "9",
			"doubao-2.1-lite":   "seed-lite-7b",
			"deepseek-v4-flash": "person-deepseek-v4-1-flash-261017",
		},
	}
	cases := []struct{ in, want string }{
		{"9", "auto"},
		{"seed-lite-7b", "doubao-2.1-lite"},
		{"person-deepseek-v4-1-flash-261017", "deepseek-v4-flash"},
		{"deepseek-v4-flash", "deepseek-v4-flash"}, // 传进来已是可读名，原样返回
		{"unknown-key", "unknown-key"},             // 解析不到不编造
		{"", ""},
	}
	for _, tc := range cases {
		if got := c.ModelLabel(tc.in); got != tc.want {
			t.Errorf("ModelLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
