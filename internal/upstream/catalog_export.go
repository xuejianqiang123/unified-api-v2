package upstream

// StaticContextCap 暴露静态知识表里的模型能力（供 providercap 按 provider 组合使用）。
//
// 只读 contextCapFallback：不含运行时拉取、不触发 models.dev、不写缓存 ——
// 调用方（能力矩阵）需要的是一个**确定、同步、零副作用**的下限值。
//
// ok=false 表示静态表未收录该模型（此时调用方应视为「未知」，不得当成 0）。
func StaticContextCap(model string) (contextLength, maxOutput int64, ok bool) {
	e, ok := contextCapFallback[model]
	if !ok {
		return 0, 0, false
	}
	return e.context, e.maxOutput, true
}
