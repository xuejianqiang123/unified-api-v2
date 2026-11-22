package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"unified-api-v2/internal/providercap"
)

// loadTraeCapabilities 从 trae2api 的 /v1/models 拉 context_window 填进能力矩阵。
//
// 为什么在这里做而不在 providercap 包内：包保持纯静态（可测试、无网络），
// 网络依赖留在装配层。失败静默降级——拉不到就是「能力未知」，与之前一致。
func loadTraeCapabilities(base string) {
	if base == "" {
		return
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(base + "/v1/models")
	if err != nil {
		log.Printf("trae 能力矩阵：拉取模型列表失败（按未知处理）: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("trae 能力矩阵：/v1/models 返回 %d（按未知处理）", resp.StatusCode)
		return
	}
	var doc struct {
		Data []struct {
			ID            string `json:"id"`
			ContextWindow int64  `json:"context_window"`
			AliasFor      string `json:"alias_for"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		log.Printf("trae 能力矩阵：解析失败（按未知处理）: %v", err)
		return
	}
	n := 0
	for _, m := range doc.Data {
		if m.ContextWindow > 0 && m.ID != "" {
			providercap.SetDynamic("trae", m.ID, m.ContextWindow)
			n++
		}
	}
	log.Printf("trae 能力矩阵：已登记 %d 个模型的上下文窗口", n)
}
