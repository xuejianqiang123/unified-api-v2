package dbserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 豆包工作模式的「活动任务」签到协议。
//
// 背景（2026-11-23 实测）：旧的 `POST /alice/ug/campaign/landing/checkin`
// （body {"scene":...}）已失效，上游返回「当前无可用签到渠道」。豆包改成了
// 活动 → 任务 → 领奖励的三段式：
//
//	POST /alice/user/launch                       拿 ug_campaign_info.campaign_group_code
//	POST /alice/ug/campaign/v3/detail/            body {"campaign_group_code":g}，读 tasks[]
//	POST /alice/ug/campaign/item/reward/claim/    body {"task_id":"","campaign_group_code":g,"campaign_item_code":"daily_signin"}
//
// 关键细节：
//   - 必须带一组固定的客户端 query 参数（aid=1044603、pc_version…），否则 404；
//   - device_id/web_id/fp 来自 **Cookie**（我们以前用 sha256(cookie)[:6] 自造，是错的）；
//   - 领取是异步的，返回 "领取结果确认中"，要回查 daily_checkin.checked_in_today 才算成功。
const (
	doubaoCampaignDetail = "/alice/ug/campaign/v3/detail/"
	doubaoCampaignList   = "/alice/ug/campaign/list/"
	doubaoCampaignClaim  = "/alice/ug/campaign/item/reward/claim/"
	doubaoLaunch         = "/alice/user/launch"
	// dailySigninCode 每日签到任务在 tasks[] 里的 campaign_code。
	dailySigninCode = "daily_signin"
)

// campaignTask 是 tasks[] 里我们关心的字段。
type campaignTask struct {
	Code  string `json:"campaign_code"`
	Group string `json:"campaign_group_code"`
	Mode  int    `json:"reward_mode"`
	State int    `json:"task_status"`
	Daily *struct {
		CheckedInToday bool   `json:"checked_in_today"`
		ContinuousDays int    `json:"continuous_days"`
		LastCheckin    string `json:"last_checkin_date"`
		Reward         struct {
			Amount int    `json:"reward_amount"`
			Type   string `json:"reward_type"`
		} `json:"reward"`
	} `json:"daily_checkin"`
}

// Claimable 判断该任务当前能否直接领取：
// task_status==2（已完成待领），或 reward_mode==2（直接领取）且状态在 1/4。
func (t campaignTask) Claimable() bool {
	if t.State == 2 {
		return true
	}
	return t.Mode == 2 && (t.State == 1 || t.State == 4)
}

// campaignQuery 构造豆包工作端要求的 query（照 PC 客户端）。
func campaignQuery(cookie string) url.Values {
	q := url.Values{
		"aid": {"1044603"}, "real_aid": {"1044603"},
		"device_platform": {"web"}, "version_code": {"20800"},
		"samantha_web": {"1"}, "web_platform": {"desktop"},
		"doubao_device_platform": {"desktop"}, "client_platform": {"pc_client"},
		"pkg_type": {"release_version"}, "runtime": {"web"},
		"runtime_version": {"3.42.2"}, "pc_version": {"2.33.5"},
		"doubao_pc_version": {"2.33.5"}, "use-olympus-account": {"1"},
		"language": {"zh"}, "region": {"CN"}, "sys_region": {"CN"},
	}
	// device_id / web_id / fp 必须从 Cookie 里取——自造的值会被上游判为非法设备。
	for k, v := range cookieMap(cookie) {
		switch k {
		case "device_id", "web_id":
			q.Set(k, v)
		case "s_v_web_id":
			q.Set("fp", v)
		}
	}
	return q
}

func cookieMap(cookie string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(cookie, "; ") {
		if i := strings.Index(part, "="); i > 0 {
			out[part[:i]] = part[i+1:]
		}
	}
	return out
}

// postCampaign 向豆包工作端点发 JSON POST，返回解析后的对象。
func (s *Server) postCampaign(cookie, path string, body any) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	u := "https://www.doubao.com" + path + "?" + campaignQuery(cookie).Encode()
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://www.doubao.com/")
	req.Header.Set("User-Agent", s.cfg.Upstream.UserAgent)
	req.Header.Set("Cookie", cookie)

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("响应非 JSON: %s", truncate(string(data), 160))
	}
	return out, nil
}

// campaignGroup 取该账号当前的活动组（没有返回空串）。
func (s *Server) campaignGroup(cookie string) (string, error) {
	launch, err := s.postCampaign(cookie, doubaoLaunch, map[string]any{})
	if err != nil {
		return "", err
	}
	data, _ := launch["data"].(map[string]any)
	info, _ := data["ug_campaign_info"].(map[string]any)
	g, _ := info["campaign_group_code"].(string)
	return g, nil
}

// dailySigninTask 读活动详情，返回每日签到任务（没有返回 nil）。
func (s *Server) dailySigninTask(cookie, group string) (*campaignTask, error) {
	path := doubaoCampaignList
	if group == "pc_incentive_task_mvp_v3" {
		path = doubaoCampaignDetail
	}
	out, err := s.postCampaign(cookie, path, map[string]any{"campaign_group_code": group})
	if err != nil {
		return nil, err
	}
	data, _ := out["data"].(map[string]any)
	tasks, _ := data["tasks"].([]any)
	for _, raw := range tasks {
		b, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var t campaignTask
		if err := json.Unmarshal(b, &t); err != nil {
			continue
		}
		if t.Code == dailySigninCode {
			if t.Group == "" {
				t.Group = group
			}
			return &t, nil
		}
	}
	return nil, nil
}

// doCheckin 走「活动任务」模型签到。
func (s *Server) doCheckin(name, cookie, dev string) checkinResult {
	res := checkinResult{Name: name, TS: time.Now().Unix()}
	if cookie == "" {
		res.Status, res.Message = "error", "无 Cookie"
		return res
	}
	_ = dev // device_id 现在从 Cookie 取，保留形参兼容旧调用

	group, err := s.campaignGroup(cookie)
	if err != nil {
		res.Status, res.Message = "error", truncate(err.Error(), 200)
		return res
	}
	if group == "" {
		res.Status, res.Message = "no_campaign", "此账号当前没有每日签到活动"
		return res
	}

	task, err := s.dailySigninTask(cookie, group)
	if err != nil {
		res.Status, res.Message = "error", truncate(err.Error(), 200)
		return res
	}
	if task == nil || task.Daily == nil {
		res.Status, res.Message = "no_campaign", "活动里没有每日签到任务"
		return res
	}
	res.Days = task.Daily.ContinuousDays
	if task.Daily.CheckedInToday {
		res.Status, res.Message = "already", "今日已签到"
		return res
	}
	if !task.Claimable() {
		res.Status, res.Message = "error", "今日任务尚未满足领取条件"
		return res
	}

	// 领取（异步）：先提交，再回查。
	claim, cerr := s.postCampaign(cookie, doubaoCampaignClaim, map[string]any{
		"task_id":             "",
		"campaign_group_code": task.Group,
		"campaign_item_code":  task.Code,
	})
	if cerr != nil {
		res.Status, res.Message = "error", truncate(cerr.Error(), 200)
		return res
	}
	biz, _ := claim["data"].(map[string]any)
	if e, ok := biz["biz_error"].(map[string]any); ok {
		// "领取结果确认中" 属正常（异步），继续回查；其他错误如实上报。
		if msg, _ := e["message"].(string); msg != "" && !strings.Contains(msg, "确认中") {
			res.Status, res.Message = "error", msg
			return res
		}
		if msg, _ := e["message"].(string); msg != "" {
			res.Message = msg
		}
	}

	// 回查：只有 checked_in_today 变 true 才算成功。
	if after, err := s.dailySigninTask(cookie, group); err == nil && after != nil && after.Daily != nil {
		if after.Daily.CheckedInToday {
			res.Status = "ok"
			res.Days = after.Daily.ContinuousDays
			if after.Daily.Reward.Amount > 0 {
				res.Reward = fmt.Sprintf("%d 额度", after.Daily.Reward.Amount)
			}
			if res.Message == "" {
				res.Message = "签到成功"
			}
			return res
		}
	}
	res.Status = "pending"
	res.Message = "领取结果确认中，未确认成功"
	return res
}
