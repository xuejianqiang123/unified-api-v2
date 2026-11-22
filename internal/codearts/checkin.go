package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// 签到相关端点（snap-access，与 chat 同一 host、但**另一套签名口径**）。
const (
	OpsDeliveryPath = "/v1/ops/delivery"
	OpsClaimPath    = "/v1/ops/claim"
	OpsConfirmPath  = "/v1/ops/confirm"
	// OpsChannel 是 IDE 渠道标识，请求参数与领取体都要带。
	OpsChannel = "IDE"
	// dailyLoginType 是「每日签到」活动的 type 值。
	dailyLoginType = "USER_LOGIN"
	// alreadyClaimedCode 是服务端的幂等业务码：今天已经领过。
	// 判成 failed 会让用户看到一个并不存在的故障，所以必须单列。
	alreadyClaimedCode = 40001
)

// claimedStatuses 是「已领过」的状态集合（服务端在 claimable 未刷新时用它们表达）。
var claimedStatuses = map[string]bool{"CLAIMED": true, "CONFIRMED": true, "CONSUMED": true}

// Activity 是一条运营活动。
type Activity struct {
	// CampaignID 领取时要回传。服务端下发的是**数字**（实测 1），
	// 只按字符串解析会得到空串，进而被判成「活动缺少 campaignId」。
	CampaignID string
	Type       string
	Title      string
	Claimable  bool
	Status     string
	Amount     int64
}

// ClaimKind 是签到结果分类。每一种对用户的含义不同，不能合并。
type ClaimKind string

const (
	// ClaimClaimed 本次真的领到了。
	ClaimClaimed ClaimKind = "claimed"
	// ClaimAlready 今天已经领过（含服务端 40001 幂等码）。
	ClaimAlready ClaimKind = "already-claimed"
	// ClaimInactive 活动存在但当前不可领（未开始/已结束/非积分账户）。
	ClaimInactive ClaimKind = "inactive"
	// ClaimFailed 查询或领取失败（网络/凭据/解析）。
	ClaimFailed ClaimKind = "failed"
)

// ClaimResult 是一次签到的结果。
type ClaimResult struct {
	Kind    ClaimKind
	Credit  int64  // 本次发放的积分数（读不到时为 0，绝不臆造）
	Message string // 给运维看的原因，空表示无需说明
}

// Activities 查活动列表。
//
// 走 DoOps（Agent-Type 不进签名）：这一批端点与 chat 是两套口径。
func (c *Client) Activities(ctx context.Context) ([]Activity, error) {
	path := OpsDeliveryPath + "?channel=" + OpsChannel
	resp, err := c.DoOps(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	data, err := unwrapSnapEnvelope(resp.StatusCode, raw)
	if err != nil {
		return nil, err
	}
	items, ok := data["items"].([]any)
	if !ok {
		return nil, fmt.Errorf("codearts: 活动列表响应缺少 items 字段")
	}
	out := make([]Activity, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, Activity{
			CampaignID: campaignIDOf(m),
			Type:       readString(m, "type"),
			Title:      readString(m, "title"),
			Claimable:  readBool(m, "claimable"),
			Status:     readString(m, "status"),
			Amount:     readInt64(m, "benefitAmount"),
		})
	}
	return out, nil
}

// ClaimDailyCheckin 执行一次完整的每日签到。
//
// 顺序严格按上游实测：
//  1. 活动列表是**权威**（不看账户是否积分制 —— 体验版账号没有积分 metric
//     但活动可领，用 metric 当门禁会得到假阴性）；
//  2. claimable=false 时按 status 区分「已领」与「不可领」；
//  3. claim 的成功判据是信封 code；40001 单独判为已领；
//  4. 响应 data.id 非 null 时补 confirm（漏了积分会停在待确认）；
//  5. confirm 失败**不**翻盘成 failed —— 积分已在待确认态，报失败会让用户重复点击。
func (c *Client) ClaimDailyCheckin(ctx context.Context) (ClaimResult, error) {
	acts, err := c.Activities(ctx)
	if err != nil {
		return ClaimResult{Kind: ClaimFailed, Message: "活动列表查询失败: " + err.Error()}, err
	}
	var daily *Activity
	for i := range acts {
		if acts[i].Type == dailyLoginType {
			daily = &acts[i]
			break
		}
	}
	if daily == nil {
		return ClaimResult{Kind: ClaimInactive, Message: "未找到每日签到活动"}, nil
	}
	if !daily.Claimable {
		if claimedStatuses[daily.Status] {
			return ClaimResult{Kind: ClaimAlready, Message: "今天已领取"}, nil
		}
		return ClaimResult{Kind: ClaimInactive, Message: "当前不可领取（status=" + daily.Status + "）"}, nil
	}
	if daily.CampaignID == "" {
		return ClaimResult{Kind: ClaimFailed, Message: "活动缺少 campaignId，无法领取"}, nil
	}

	claimBody, _ := json.Marshal(map[string]any{
		"campaignId": daily.CampaignID,
		"channel":    OpsChannel,
	})
	resp, err := c.DoOps(ctx, http.MethodPost, OpsClaimPath, claimBody)
	if err != nil {
		return ClaimResult{Kind: ClaimFailed, Message: "领取请求失败: " + err.Error()}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	data, err := unwrapSnapEnvelope(resp.StatusCode, raw)
	if err != nil {
		// 40001 在信封 code 里，不在 HTTP 状态里 —— 但 unwrap 会把非 0 业务码
		// 变成错误，所以这里要认出它。
		if bizCodeOf(raw) == alreadyClaimedCode {
			return ClaimResult{Kind: ClaimAlready, Message: "今天已领取"}, nil
		}
		return ClaimResult{Kind: ClaimFailed, Message: "领取失败: " + err.Error()}, nil
	}

	// 服务端要求确认时才补 confirm（判据：benefit.id 非 null）。
	if id, ok := data["id"]; ok && id != nil {
		confirmBody, _ := json.Marshal(map[string]any{"campaignId": daily.CampaignID})
		cresp, cerr := c.DoOps(ctx, http.MethodPost, OpsConfirmPath, confirmBody)
		if cerr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(cresp.Body, 64<<10))
			cresp.Body.Close()
		}
		// 确认失败不翻盘：积分已进入待确认态，报 failed 会让用户以为没领到。
	}

	credit := firstInt64(data,
		"benefitAmount", "credit", "credits", "creditAmount", "amount")
	if credit <= 0 {
		credit = daily.Amount
	}
	msg := ""
	if credit <= 0 {
		msg = "已领取，但上游没有下发本次积分数量"
	}
	return ClaimResult{Kind: ClaimClaimed, Credit: credit, Message: msg}, nil
}

// unwrapSnapEnvelope 解 snap-access 的信封：有 code 时非 0 即失败，无 code 时 body 本身即数据。
func unwrapSnapEnvelope(status int, raw []byte) (map[string]any, error) {
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", status, truncateForErr(string(raw)))
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("响应不是 JSON: %v", err)
	}
	if code, ok := obj["code"]; ok {
		if n, isNum := toInt64OK(code); isNum && n != 0 {
			msg := readString(obj, "message")
			if msg == "" {
				msg = readString(obj, "msg")
			}
			if msg == "" {
				msg = "业务码 " + strconv.FormatInt(n, 10)
			}
			return nil, fmt.Errorf("%s（code=%d）", msg, n)
		}
		data, ok := obj["data"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("响应缺少 data 字段")
		}
		return data, nil
	}
	return obj, nil
}

// bizCodeOf 只读信封里的 code（错误路径用）。
func bizCodeOf(raw []byte) int64 {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return 0
	}
	if n, ok := toInt64OK(obj["code"]); ok {
		return n
	}
	return 0
}

func campaignIDOf(m map[string]any) string {
	// 数字与字符串两种形态都吃（实测是数字 1）。
	if s := readString(m, "campaignId"); s != "" {
		return s
	}
	if v, ok := m["campaignId"]; ok {
		if n, isNum := toInt64OK(v); isNum {
			return strconv.FormatInt(n, 10)
		}
	}
	return ""
}

func readString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func readBool(m map[string]any, key string) bool {
	v, ok := m[key].(bool)
	return ok && v
}

func readInt64(m map[string]any, key string) int64 {
	n, _ := toInt64OK(m[key])
	return n
}

func firstInt64(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if n, ok := toInt64OK(m[k]); ok && n > 0 {
			return n
		}
	}
	return 0
}

func toInt64OK(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}
