// aliasnorm.go 别名归并：把「同一个模型的不同写法」收拢成一条多候选别名。
//
// 背景（用户 2026-11-22 明确要求）：`glm-5.3-flash` / `GLM-5.3-Flash` /
// `codearts-glm-5.3-flash` 在业务上是同一个模型，却因为大小写与 provider 前缀
// 被拆成三条别名，各自只挂一家 —— 结果「明明五家都有，却不会自动切换」。
//
// 归并规则只做两件事，**刻意保守**：
//  1. 转小写（上游大小写不统一：codearts 的 GLM-5.2 是大写、glm-5.3 是小写）；
//  2. 去掉已知 provider 前缀（codearts- / loomy- / doubao- / workbuddy- / trae-）。
//
// 不做「. 与 - 互换」「v 前缀归一」这类模糊匹配：`deepseek-v4.1-flash` 与
// `deepseek-v4-flash` 是**不同版本**，误合并会把请求静默发到错的模型上。
package server

import (
	"log"
	"sort"
	"strings"
)

// providerPrefixes 是会被从别名里剥掉的前缀（由注册表派生，不硬编码）。
//
// 注意剥离顺序：按长度倒序，避免 `workbuddy-` 被 `w` 之类短前缀误伤。
func (h *Handler) providerPrefixes() []string {
	names := make([]string, 0, len(h.providers))
	for n := range h.providers {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, n+"-")
	}
	return out
}

// normalizeAliasKey 把别名归一到「比较用」的形态。
//
// 返回空串表示该名字无法归一（全空白），调用方应保持原样。
func normalizeAliasKey(name string, prefixes []string) string {
	k := strings.ToLower(strings.TrimSpace(name))
	if k == "" {
		return ""
	}
	for _, p := range prefixes {
		if strings.HasPrefix(k, p) && len(k) > len(p) {
			k = k[len(p):]
			break
		}
	}
	return k
}

// mergeAliasVariants 把归一后相同的别名合并成一条多候选别名。
//
// 关键点：合并后每个 provider 仍用它**自己的**模型名（`ModelByProvider`）——
// loomy 叫 `GLM-5.3-Flash`、doubao 叫 `glm-5.3-flash`，同名不同形，不能互相顶替。
//
// 返回的新 map 里，**每个原始 key 都指向合并后的同一条别名**：
// 这样客户端写哪种写法都能路由到全量候选，而不是「写对大小写才有多家」。
func mergeAliasVariants(in map[string]UnifiedAlias, prefixes []string) map[string]UnifiedAlias {
	if len(in) == 0 {
		return in
	}

	groups := make(map[string][]string, len(in))
	for name := range in {
		k := normalizeAliasKey(name, prefixes)
		if k == "" {
			k = name
		}
		groups[k] = append(groups[k], name)
	}

	out := make(map[string]UnifiedAlias, len(in))
	for _, names := range groups {
		if len(names) == 1 {
			out[names[0]] = in[names[0]]
			continue
		}
		sort.Strings(names)

		merged := UnifiedAlias{
			Failover:        true,
			ModelByProvider: map[string]string{},
		}
		var providers []string
		seenProvider := map[string]bool{}
		for _, name := range names {
			a := in[name]
			for _, p := range a.Candidates() {
				if !seenProvider[p] {
					seenProvider[p] = true
					providers = append(providers, p)
				}
				// 该家用它自己声明的模型名；同一家出现多次时保留第一次。
				if _, ok := merged.ModelByProvider[p]; !ok {
					merged.ModelByProvider[p] = a.Model
				}
			}
			if merged.Name == "" {
				merged.Name = a.Name
			}
			if merged.Require == nil {
				merged.Require = a.Require
			}
			if merged.ContextLength == 0 {
				merged.ContextLength = a.ContextLength
			}
		}
		merged.Providers = providers
		// 规范名取「小写且不带前缀」的那个（没有就用首字母序第一个）。
		merged.Model = in[canonicalAliasName(names, prefixes)].Model
		if len(providers) <= 1 {
			merged.Failover = false
		}

		log.Printf("别名归并：%v → %d 家候选 %v", names, len(providers), providers)
		for _, name := range names {
			out[name] = merged
		}
	}
	return out
}

// canonicalAliasName 在一组等价别名里挑规范名：优先「已全小写且未被剥前缀」的那个。
func canonicalAliasName(names []string, prefixes []string) string {
	for _, n := range names {
		if n == strings.ToLower(n) && normalizeAliasKey(n, prefixes) == n {
			return n
		}
	}
	return names[0]
}

// ModelFor 返回某个 provider 该用的上游模型名。
//
// 归并后的别名里，每家可能叫法不同（大小写/前缀），必须逐家取，
// 不能共用单一的 Model 字段。
func (a UnifiedAlias) ModelFor(provider string) string {
	if m, ok := a.ModelByProvider[provider]; ok && m != "" {
		return m
	}
	return a.Model
}
