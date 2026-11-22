package codearts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestClient 起一个假上游，并把 client 的 baseURL 指过去。
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	credPath := filepath.Join(dir, "accounts.json")
	raw, _ := json.Marshal(map[string]any{"accounts": []Credential{{
		AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "st",
	}}})
	if err := os.WriteFile(credPath, raw, 0o600); err != nil {
		t.Fatalf("写凭据: %v", err)
	}
	c := NewClient(credPath)
	c.baseURL = srv.URL
	return c
}

// TestCampaignIDNumeric 是最容易踩的一格：服务端下发数字 1，
// 只按字符串解析会得到空串，进而被判「活动缺少 campaignId」。
func TestCampaignIDNumeric(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"数字", map[string]any{"campaignId": float64(1)}, "1"},
		{"字符串", map[string]any{"campaignId": "42"}, "42"},
		{"缺失", map[string]any{}, ""},
	}
	for _, c := range cases {
		if got := campaignIDOf(c.in); got != c.want {
			t.Errorf("%s: campaignIDOf = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestUnwrapSnapEnvelope(t *testing.T) {
	// 裸对象（statistics/plugin 形态）：无 code，body 本身即数据。
	obj, err := unwrapSnapEnvelope(200, []byte(`{"metrics":[]}`))
	if err != nil || obj["metrics"] == nil {
		t.Errorf("裸对象应原样返回: obj=%v err=%v", obj, err)
	}

	// 信封 code=0 + data。
	obj, err = unwrapSnapEnvelope(200, []byte(`{"code":0,"data":{"items":[]}}`))
	if err != nil || obj["items"] == nil {
		t.Errorf("信封应解出 data: obj=%v err=%v", obj, err)
	}

	// 信封 code=40001：必须是错误（但调用方要能认出它）。
	if _, err := unwrapSnapEnvelope(200, []byte(`{"code":40001,"message":"尚未到达权益刷新时间"}`)); err == nil {
		t.Error("40001 应返回错误")
	}
	if got := bizCodeOf([]byte(`{"code":40001}`)); got != 40001 {
		t.Errorf("bizCodeOf = %d, want 40001", got)
	}

	// 非 2xx。
	if _, err := unwrapSnapEnvelope(401, []byte(`{"error_code":"APIG.0301"}`)); err == nil {
		t.Error("401 应返回错误")
	}
}

// TestClaimAlreadyByStatus 走「活动列表说已领」这一格 —— 这是主要出口，
// 不能因为 claimable=false 就报 failed（那会让用户看到不存在的故障）。
func TestClaimAlreadyByStatus(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, OpsDeliveryPath) {
			t.Errorf("意外路径 %s", r.URL.Path)
		}
		// Agent-Type 必须**不**出现在 SignedHeaders 里（两套口径）。
		if auth := r.Header.Get("Authorization"); strings.Contains(auth, "agent-type") ||
			strings.Contains(auth, "Agent-Type") {
			t.Errorf("ops 端点的 Agent-Type 不应进签名: %s", auth)
		}
		if r.Header.Get("Agent-Type") != opsAgentType {
			t.Errorf("Agent-Type 头缺失，实际 %q", r.Header.Get("Agent-Type"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"items": []any{
				map[string]any{"campaignId": 1, "type": "USER_LOGIN", "status": "CLAIMED", "claimable": false},
			}},
		})
	})

	res, err := c.ClaimDailyCheckin(context.Background())
	if err != nil {
		t.Fatalf("ClaimDailyCheckin: %v", err)
	}
	if res.Kind != ClaimAlready {
		t.Errorf("Kind = %s, want %s", res.Kind, ClaimAlready)
	}
}

// TestClaimThenConfirm 覆盖成功路径：claimable=true → claim 返回 id → 必须补 confirm。
func TestClaimThenConfirm(t *testing.T) {
	var claimed, confirmed bool
	var claimBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case OpsDeliveryPath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"items": []any{
					map[string]any{"campaignId": 1, "type": "USER_LOGIN", "status": "", "claimable": true, "benefitAmount": 100},
				}},
			})
		case OpsClaimPath:
			claimed = true
			_ = json.NewDecoder(r.Body).Decode(&claimBody)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "data": map[string]any{"id": "benefit-1", "benefitAmount": 100},
			})
		case OpsConfirmPath:
			confirmed = true
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	})

	res, err := c.ClaimDailyCheckin(context.Background())
	if err != nil {
		t.Fatalf("ClaimDailyCheckin: %v", err)
	}
	if res.Kind != ClaimClaimed || res.Credit != 100 {
		t.Errorf("结果 = %+v, want claimed/100", res)
	}
	if !claimed || !confirmed {
		t.Errorf("claimed=%v confirmed=%v，两步行都必须发生", claimed, confirmed)
	}
	// campaignId 必须原样回传成字符串形态（服务端下发数字，回传要能对上）。
	if got := claimBody["campaignId"]; got != "1" {
		t.Errorf("claim 的 campaignId = %v, want \"1\"", got)
	}
	if claimBody["channel"] != OpsChannel {
		t.Errorf("claim 的 channel = %v, want %s", claimBody["channel"], OpsChannel)
	}
}

// TestClaimIdempotentCode40001 HTTP 200 但业务码 40001 ⇒ already-claimed，不是 failed。
func TestClaimIdempotentCode40001(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case OpsDeliveryPath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"items": []any{
					map[string]any{"campaignId": 1, "type": "USER_LOGIN", "claimable": true},
				}},
			})
		case OpsClaimPath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 40001, "message": "尚未到达权益刷新时间，暂时无法领取", "data": nil,
			})
		}
	})

	res, err := c.ClaimDailyCheckin(context.Background())
	if err != nil {
		t.Fatalf("ClaimDailyCheckin: %v", err)
	}
	if res.Kind != ClaimAlready {
		t.Errorf("Kind = %s, want already-claimed（40001 不是故障）", res.Kind)
	}
}

// TestClaimNoDailyActivity 活动列表里没有 USER_LOGIN ⇒ inactive（不是 failed）。
func TestClaimNoDailyActivity(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"items": []any{
				map[string]any{"campaignId": 2, "type": "NEW_USER_REGISTER", "status": "CLAIMED"},
			}},
		})
	})
	res, err := c.ClaimDailyCheckin(context.Background())
	if err != nil {
		t.Fatalf("ClaimDailyCheckin: %v", err)
	}
	if res.Kind != ClaimInactive {
		t.Errorf("Kind = %s, want inactive", res.Kind)
	}
}
