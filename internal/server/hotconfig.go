// hotconfig.go 别名表的热载入口。
//
// 为什么只热载别名：别名是最常改的东西（新增模型、调候选顺序），
// 而 listen / providers 接线 / api_key 变更会改变服务形态，必须重启。
// 把「能热载」与「必须重启」分开，比笼统地"支持热载"安全。
package server

import (
	"log"
	"sort"
)

// Aliases 返回当前生效的别名表（只读视图）。
//
// 调用方**不得**修改返回的 map：它可能被并发请求共享。需要改动请走 ReloadAliases。
func (h *Handler) Aliases() map[string]UnifiedAlias {
	p := h.aliases.Load()
	if p == nil {
		return nil
	}
	return *p
}

// ReloadAliases 原子替换别名表，返回 (新增数, 删除数, 变更数)。
//
// 替换是原子的：正在处理的请求要么看到旧表、要么看到新表，不会看到半张表。
func (h *Handler) ReloadAliases(next map[string]UnifiedAlias) (added, removed, changed int) {
	// 先归并「同一个模型的多种写法」（大小写 / provider 前缀），
	// 让每种写法都路由到全量候选 —— 这是用户明确要求的自动切换前提。
	if len(next) > 0 {
		next = mergeAliasVariants(next, h.providerPrefixes())
	}
	prev := h.Aliases()
	for name, a := range next {
		old, ok := prev[name]
		if !ok {
			added++
			continue
		}
		if !sameAlias(old, a) {
			changed++
		}
	}
	for name := range prev {
		if _, ok := next[name]; !ok {
			removed++
		}
	}
	// 复制一份再存：调用方的 map 之后可能被它自己复用/修改。
	m := make(map[string]UnifiedAlias, len(next))
	for k, v := range next {
		m[k] = v
	}
	h.aliases.Store(&m)

	if added+removed+changed > 0 {
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		log.Printf("配置热载：别名 +%d -%d ~%d（当前 %d 条）", added, removed, changed, len(names))
	}
	return added, removed, changed
}

// sameAlias 比较两个别名是否等价（只看影响路由的字段）。
func sameAlias(a, b UnifiedAlias) bool {
	if a.Model != b.Model || a.Failover != b.Failover || a.Name != b.Name {
		return false
	}
	ac, bc := a.Candidates(), b.Candidates()
	if len(ac) != len(bc) {
		return false
	}
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	if (a.Require == nil) != (b.Require == nil) {
		return false
	}
	if a.Require != nil && b.Require != nil {
		if a.Require.Context != b.Require.Context {
			return false
		}
		if (a.Require.Images == nil) != (b.Require.Images == nil) {
			return false
		}
		if a.Require.Images != nil && *a.Require.Images != *b.Require.Images {
			return false
		}
	}
	return true
}
