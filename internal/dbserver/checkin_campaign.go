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

// 豆包签到：网页端 landing/checkin。
//
// 2026-11-23 实测结论（踩过的三个坑都写在这里）：
//
//  1. 接口没变，还是 POST /alice/ug/campaign/landing/checkin，
//     body {"scene":"pc_incentive_task_mvp_v3","biz_params":{}}。
//  2. **参数必须是网页端那一套**：aid=497858、doubao_device_platform=web、
//     pc_version=2.36.14、doubao_pc_version=5.75.58、runtime_version=5.75.58。
//     用 PC 客户端的 aid=1044603 / desktop / 2.33.5 会得到「当前无可用签到渠道」。
//  3. **msToken 是必需的**，从 Cookie 里取。缺失时上游回 710010702「系统错误」，
//     这个错误码极具误导性（看起来像服务端故障，实际是缺 token）。
//
// 所以「无可用签到渠道」从来不是活动下线，而是参数与 token 不对。
const landingCheckinPath = "/alice/ug/campaign/landing/checkin"

// landingResp 是 landing/checkin 的响应体。
type landingResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data *struct {
		CheckedIn      bool   `json:"checked_in"`
		CheckinDate    string `json:"checkin_date"`
		ContinuousDays int    `json:"continuous_days"`
		Idempotent     bool   `json:"idempotent"`
		RewardAmount   int    `json:"reward_amount"`
		RewardCode     string `json:"reward_code"`
	} `json:"data"`
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

// checkinQuery 构造网页端签到参数（与浏览器实际请求一致）。
func checkinQuery(cm map[string]string) url.Values {
	q := url.Values{
		"version_code":           {"20800"},
		"language":               {"zh"},
		"device_platform":        {"web"},
		"doubao_device_platform": {"web"},
		"aid":                    {"497858"},
		"real_aid":               {"497858"},
		"pkg_type":               {"release_version"},
		"pc_version":             {"2.36.14"},
		"doubao_pc_version":      {"5.75.58"},
		"runtime_version":        {"5.75.58"},
		"runtime":                {"web"},
		"samantha_web":           {"1"},
		"web_platform":           {"desktop"},
		"use-olympus-account":    {"1"},
	}
	// 这三个都从 Cookie 取；msToken 缺了直接失败，不猜、不自造。
	for _, k := range []string{"msToken", "web_id", "tea_uuid"} {
		if v := cm[k]; v != "" {
			q.Set(k, v)
		}
	}
	if fp := cm["s_v_web_id"]; fp != "" {
		q.Set("fp", fp)
	}
	return q
}

// doCheckin 走网页端 landing/checkin 签到。
func (s *Server) doCheckin(name, cookie, dev string) checkinResult {
	res := checkinResult{Name: name, TS: time.Now().Unix()}
	if cookie == "" {
		res.Status, res.Message = "error", "无 Cookie"
		return res
	}
	_ = dev

	cm := cookieMap(cookie)
	if cm["msToken"] == "" {
		// 明确说清原因：这个错误以前会被误判成「上游挂了」。
		res.Status, res.Message = "error",
			"Cookie 缺少 msToken（需在浏览器登录豆包后重新导出 Cookie）"
		return res
	}

	scene := s.cfg.Checkin.Scene
	if scene == "" {
		scene = "pc_incentive_task_mvp_v3"
	}
	body, _ := json.Marshal(map[string]any{"scene": scene, "biz_params": map[string]any{}})
	u := "https://www.doubao.com" + landingCheckinPath + "?" + checkinQuery(cm).Encode()

	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		res.Status, res.Message = "error", err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://www.doubao.com/pc/agent-explore?enter_method=fixed_entry")
	req.Header.Set("User-Agent", s.cfg.Upstream.UserAgent)
	req.Header.Set("Cookie", cookie)

	resp, err := s.http.Do(req)
	if err != nil {
		res.Status, res.Message = "error", err.Error()
		return res
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var payload landingResp
	_ = json.Unmarshal(raw, &payload)

	if payload.Data == nil || payload.Code != 0 {
		code := payload.Code
		msg := payload.Msg
		if msg == "" {
			msg = truncate(string(raw), 160)
		}
		// 710010702 在缺 msToken / token 过期时都会出现，给出可操作提示。
		if code == 710010702 {
			msg = "上游返回 710010702（设备或 msToken 校验失败）：请重新登录豆包并更新 Cookie"
		}
		res.Status, res.Message = "error", fmt.Sprintf("%s (code %d)", msg, code)
		return res
	}

	d := payload.Data
	res.Days = d.ContinuousDays
	if !d.CheckedIn {
		res.Status, res.Message = "error", "上游未返回签到成功"
		return res
	}
	if d.RewardAmount > 0 {
		res.Reward = fmt.Sprintf("%d 额度", d.RewardAmount)
	}
	if d.Idempotent {
		// 幂等：本次之前就已签到（含重试/重复触发），不算新签。
		res.Status, res.Message = "already", "今日已签到"
		return res
	}
	res.Status, res.Message = "ok", "签到成功"
	return res
}
