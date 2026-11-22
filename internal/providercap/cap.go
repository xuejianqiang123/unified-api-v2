// Package providercap 按 (provider, model) 索引的模型能力矩阵。
//
// 为什么必须带 provider 维度：同一个模型名在不同家的容量可以差 3.5 倍 ——
// 实测 glm-5.3 在 doubao 是 300,000、在 workbuddy 是 1,000,000、在 codearts 是
// 1,048,576。一个 800K 的对话在主家正常，failover 到小窗口那家立刻 400。
// 只按模型名存（如 internal/upstream 的全局表）无法表达这件事。
//
// 数据置信度分四级，**未知不等于不满足**：Lookup 返回 ok=false 时调用方必须
// 放行，不能因为「查不到」就把候选跳过 —— 那会把「还没测」变成「不能用」。
package providercap

import (
	"strings"
	"sync"

	"unified-api-v2/internal/upstream"
)

// Confidence 表示一个能力条目的可信程度。
type Confidence uint8

const (
	// Unknown 完全没有数据（C/D 级）：不做任何长度校验。
	Unknown Confidence = iota
	// Partial 只知道部分能力（如是否支持图片），不知 context。
	Partial
	// Static 来自仓库内静态知识表（标注了实测/models.dev 来源）。
	Static
	// Upstream 由 provider 自己的接口返回（最可信）。
	Upstream
)

func (c Confidence) String() string {
	switch c {
	case Upstream:
		return "upstream"
	case Static:
		return "static"
	case Partial:
		return "partial"
	default:
		return "unknown"
	}
}

// Cap 一个 (provider, model) 的能力快照。
//
// Context=0 表示长度未知（不是「零上下文」）；MaxOutput=0 表示输出上限未知；
// ImagesKnown=false 表示连「支不支持图片」都不知道。
type Cap struct {
	Context     int64
	MaxOutput   int64
	Images      bool
	ImagesKnown bool
	Confidence  Confidence
	// Source 记录数据出处，便于排查「为什么这个值和我看到的不一样」。
	Source string
}

// entry 是表里的原始条目（布尔用指针区分「不支持」与「不知道」）。
type entry struct {
	context    int64
	maxOutput  int64
	images     *bool
	confidence Confidence
	source     string
}

// 表按 provider → 归一化模型名 两级索引。
//
// 归一化 = 转小写。上游的大小写不统一（codearts 的 GLM-5.2 是大写、
// glm-5.3 是小写），但同一家内部不会出现「只靠大小写区分」的两个模型。
var table = map[string]map[string]entry{
	// ── CodeArts：A 级，GET /v1/model/builtin 实测（2026-11-22）──────────────
	"codearts": {
		"glm-5.3":                  {1048576, 65536, boolPtr(false), Upstream, "codearts:/v1/model/builtin"},
		"glm-5.3-flash":            {1048576, 65536, boolPtr(true), Upstream, "codearts:/v1/model/builtin"},
		"glm-5.2":                  {202752, 65536, boolPtr(false), Upstream, "codearts:/v1/model/builtin"},
		"glm-5.2-sft-harmony":      {202752, 65535, boolPtr(false), Upstream, "codearts:/v1/model/builtin"},
		"deepseek-v4.1-flash":      {1048576, 65536, boolPtr(true), Upstream, "codearts:/v1/model/builtin"},
		"deepseek-v4.1-flash-1017": {1048576, 65536, boolPtr(true), Upstream, "codearts:/v1/model/builtin"},
		"kimi-k3":                  {1048576, 65536, boolPtr(true), Upstream, "codearts:/v1/model/builtin"},
		"kimi-k3.1-pro":            {1048576, 65536, boolPtr(true), Upstream, "codearts:/v1/model/builtin"},
		"openpangu-2.0-flash":      {524288, 65535, boolPtr(false), Upstream, "codearts:/v1/model/builtin"},
		"openpangu-2.0-pro":        {524288, 65535, boolPtr(false), Upstream, "codearts:/v1/model/builtin"},
		"openpangu-2.1-pro":        {524288, 65535, boolPtr(false), Upstream, "codearts:/v1/model/builtin"},
		"speed":                    {1048576, 65536, boolPtr(false), Upstream, "codearts:/v1/model/builtin"},
	},

	// ── 豆包：A 级，doubao2api/config.json 的 model_params ────────────────────
	// total_window_size 是「输入+输出」总窗口；memory_profile 如 300k_230 表示
	// 其中约 230K 给输入。这里取 total（保守起见调用方应再留余量）。
	"doubao": {
		"auto":               {256000, 0, nil, Upstream, "doubao2api:model_params"},
		"doubao-2.1-lite":    {256000, 0, nil, Upstream, "doubao2api:model_params"},
		"doubao-2.1-pro":     {256000, 0, nil, Upstream, "doubao2api:model_params"},
		"deepseek-v4-flash":  {300000, 0, nil, Upstream, "doubao2api:model_params"},
		"deepseek-v4-pro":    {300000, 0, nil, Upstream, "doubao2api:model_params"},
		"kimi-k3":            {300000, 0, nil, Upstream, "doubao2api:model_params"},
		"kimi-k3.1-pro":      {300000, 0, nil, Upstream, "doubao2api:model_params"},
		"kimi-k3.1-pro-fast": {300000, 0, nil, Upstream, "doubao2api:model_params"},
		"glm-5.3":            {300000, 0, nil, Upstream, "doubao2api:model_params"},
		"glm-5.3-flash":      {300000, 0, nil, Upstream, "doubao2api:model_params"},
	},

	// ── Trae：C 级，只有多模态/思考档位，**没有 context** ──────────────────────
	// 只登记「已知支持图片」的那几个（来源 /v1/models 的 capabilities_by_scope），
	// context 一律留 0 = 未知，不做长度校验。
	"trae": {
		"deepseek-v4.1-flash":      {0, 0, boolPtr(true), Partial, "trae:/v1/models capabilities"},
		"deepseek-v4-pro-official": {0, 0, boolPtr(true), Partial, "trae:/v1/models capabilities"},
		"kimi-k3":                  {0, 0, boolPtr(true), Partial, "trae:/v1/models capabilities"},
		"kimi-k2.6":                {0, 0, boolPtr(true), Partial, "trae:/v1/models capabilities"},
		"qwen3.8-max":              {0, 0, boolPtr(true), Partial, "trae:/v1/models capabilities"},
		"glm-5v-turbo":             {0, 0, boolPtr(true), Partial, "trae:/v1/models capabilities"},
		"minimax-m3":               {0, 0, boolPtr(true), Partial, "trae:/v1/models capabilities"},
		"doubao-seed-code":         {0, 0, boolPtr(true), Partial, "trae:/v1/models capabilities"},
	},

	// ── Loomy：D 级，上游无模型元数据接口 ─────────────────────────────────────
	// 整家不登记任何条目：Lookup 返回 false，调用方按「未知」放行。
	"loomy": {},
}

// dynamic 是运行时覆盖层：上游能给出权威值时优先于静态表。
// 目前只有 trae 会写入（它的 /v1/models 带 context_window）。
var dynamic sync.Map // "provider\x00model" -> Cap

// SetDynamic 写入一条运行时能力（置信度视为 Upstream）。
//
// 只填 Context>0 的条目：未知不要写成 0，那会变成「窗口为零」的错误结论。
func SetDynamic(provider, model string, contextLength int64) {
	if contextLength <= 0 {
		return
	}
	key := strings.ToLower(provider) + "\x00" + strings.ToLower(model)
	dynamic.Store(key, Cap{
		Context:    contextLength,
		Confidence: Upstream,
		Source:     provider + ":/v1/models",
	})
}

// Lookup 查一个 (provider, model) 的能力。
//
// workbuddy 单独走静态知识表（internal/upstream 的 context_catalog），
// 因为那张表已有人工维护的实测来源，复制一份到这里只会漂移。
//
// ok=false 表示**该 provider+model 没有可用数据**。调用方必须按「未知」处理，
// 不得把它当成「能力为 0」而跳过候选。
func Lookup(provider, model string) (Cap, bool) {
	p := strings.ToLower(strings.TrimSpace(provider))
	m := strings.ToLower(strings.TrimSpace(model))
	if p == "" || m == "" {
		return Cap{}, false
	}

	if p == "workbuddy" {
		if ctx, maxOut, ok := upstream.StaticContextCap(model); ok {
			return Cap{
				Context:    ctx,
				MaxOutput:  maxOut,
				Confidence: Static,
				Source:     "workbuddy:context_catalog",
			}, true
		}
		// 静态表未收录：仍返回「存在但未知」，让调用方知道这家是接线了的。
		return Cap{Confidence: Unknown, Source: "workbuddy:未收录"}, false
	}

	// 运行时覆盖优先：上游自己报的值比静态表可信。
	if v, ok := dynamic.Load(p + "\x00" + m); ok {
		if c, ok := v.(Cap); ok {
			return c, true
		}
	}

	byModel, ok := table[p]
	if !ok {
		return Cap{}, false
	}
	e, ok := byModel[m]
	if !ok {
		return Cap{}, false
	}
	c := Cap{
		Context:    e.context,
		MaxOutput:  e.maxOutput,
		Confidence: e.confidence,
		Source:     e.source,
	}
	if e.images != nil {
		c.Images = *e.images
		c.ImagesKnown = true
	}
	return c, true
}

// HasContext 判断该条目是否足以做长度校验。
//
// 只有 Confidence ≥ Static 且 Context > 0 才算「知道长度」；
// Partial（trae）与 Unknown（loomy）都返回 false ⇒ 不参与 require 校验。
func (c Cap) HasContext() bool {
	return c.Context > 0 && c.Confidence >= Static
}

func boolPtr(b bool) *bool { return &b }
