// growth_view.go 成长计划页面 / 自动巡检所需的「软结果」上游请求。
//
// 参考项目 admin/backend.py 的 _growth / _request_backend_soft 返回
// {ok, status, code, msg, data} 而不是抛异常：成长域把「门槛未达」「重复抽奖」
// 「次数不足」等业务拒绝放在 HTTP 400/409 + msg 里，巡检必须能读到这些字段
// 才能做幂等判定（例如 409 duplicate 要把 pending token 清掉而不是重试扣费）。
// 现有 growthJSON / doJSON 是「非 2xx 或 code!=0 即错误」的硬语义，供调度器
// 的旅行/连登使用；这里另开一条只读/软语义通道，两者互不改动。
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// growthPlanCheckinStatusPath 签到活动状态（billing 域，POST 空体）。
const growthPlanCheckinStatusPath = "/v2/billing/meter/checkin-activity-status"

// SoftResult 成长/计费域请求的结构化结果。Data 保留原始 data 片段（可能是
// 对象、数组或 null），由调用方按端点形状解析。
type SoftResult struct {
	OK     bool            `json:"ok"`
	Status int             `json:"status"`
	Code   int             `json:"code"`
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data"`
}

// Object 把 data 当对象解析；数组/null/非法 JSON 一律返回空 map。
func (r SoftResult) Object() map[string]any {
	out := map[string]any{}
	if len(r.Data) > 0 {
		_ = json.Unmarshal(r.Data, &out)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// Array 把 data 当对象数组解析；对象/null/非法 JSON 一律返回空切片。
func (r SoftResult) Array() []map[string]any {
	out := []map[string]any{}
	if len(r.Data) > 0 {
		_ = json.Unmarshal(r.Data, &out)
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

// requestSoft 发一个带账号头的 JSON 请求，按参考项目口径返回结构化结果。
// 不把上游原始异常写进日志/持久化：只留状态码与 msg（Msg 可能被前端展示，
// 故由上层截断）。token 刷新沿用本项目的既有约定（growthJSON 同样不刷新，
// 由 keepalive/authwatch 统一维护）。
func (c *Client) requestSoft(base string, a *auth.Auth, method, path string, body any) SoftResult {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return SoftResult{Msg: "请求编码失败"}
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return SoftResult{Msg: "请求构造失败"}
	}
	c.BillingHeaders(req, a)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// 网络层错误不回显 err（可能含 URL / 内网地址），只标记失败。
		return SoftResult{Status: 0, Msg: "网络失败"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return SoftResult{Status: resp.StatusCode, Msg: "响应读取失败"}
	}
	var env struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return SoftResult{Status: resp.StatusCode, Msg: "非 JSON 响应"}
	}
	code := 0
	if s := strings.TrimSpace(string(env.Code)); s != "" && s != "null" {
		if n, err := strconv.Atoi(strings.Trim(s, `"`)); err != nil {
			// 部分上游把 code 写成字符串数字；解析不出按 -1（非 0）处理。
			code = -1
		} else {
			code = n
		}
	}
	return SoftResult{
		OK:     resp.StatusCode >= 200 && resp.StatusCode < 300 && code == 0,
		Status: resp.StatusCode,
		Code:   code,
		Msg:    env.Msg,
		Data:   env.Data,
	}
}

// GrowthRequest 成长域（chatBase，CN = copilot.tencent.com）软结果请求。
// 路径与方法由调用方给出，覆盖成长计划快照的 10 个分项与领奖/开盒/抽奖动作。
func (c *Client) GrowthRequest(a *auth.Auth, method, path string, body any) SoftResult {
	return c.requestSoft(c.chatBase(a), a, method, path, body)
}

// BillingRequest 计费域（billingBase，CN = codebuddy.cn）软结果请求。
func (c *Client) BillingRequest(a *auth.Auth, method, path string, body any) SoftResult {
	return c.requestSoft(c.billingBase(a), a, method, path, body)
}

// CheckinStatus 查询 Buddy 加油站签到活动状态（只读）。
func (c *Client) CheckinStatus(a *auth.Auth) SoftResult {
	return c.BillingRequest(a, http.MethodPost, growthPlanCheckinStatusPath, map[string]any{})
}

// ClaimDailyCheckinSoft 领取每日签到积分（软结果）；按 realm 走与
// DailyCheckin 相同的双路径 fallback（404 才换路径）。
func (c *Client) ClaimDailyCheckinSoft(a *auth.Auth) SoftResult {
	paths := c.checkinMeterPaths(a)
	var last SoftResult
	for _, path := range paths {
		last = c.BillingRequest(a, http.MethodPost, path, map[string]any{})
		if last.OK || last.Status != http.StatusNotFound {
			return last
		}
	}
	return last
}
