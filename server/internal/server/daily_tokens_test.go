package server

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"strings"
	"testing"
)

func TestDailyQuotaReturns429AndResumesAfterLimitChange(t *testing.T) {
	h, calls := fallbackFixture(t, "a")
	h.cfg.DeepseekSGFallback = false
	if err := h.cfg.Pool.SetDailyTokenLimit("a", pool.DeepseekFreeModel, 2); err != nil {
		t.Fatal(err)
	}
	if r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, true); r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, true)
	if r.Code != 429 || !strings.Contains(r.Body.String(), "daily_token_limit") || r.Header().Get("Retry-After") == "" {
		t.Fatal(r.Code, r.Body)
	}
	if len(*calls) != 1 {
		t.Fatal("quota did not block upstream calls", *calls)
	}
	h.cfg.Pool.SetDailyTokenLimit("a", pool.DeepseekFreeModel, 0)
	if r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, false); r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
}
func TestDailyFreeQuotaRedirectAndPaidQuota(t *testing.T) {
	h, calls := fallbackFixture(t, "a", "b")
	for _, uid := range []string{"a", "b"} {
		h.cfg.Pool.SetDailyTokenLimit(uid, pool.DeepseekFreeModel, 100)
		h.cfg.Pool.RecordTokenUsage(uid, pool.TokenUsageDelta{Model: pool.DeepseekFreeModel, HasTotalTokens: true, TotalTokens: 100})
	}
	if r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, false); r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	if len(*calls) != 1 || (*calls)[0].model != pool.DeepseekPaidModel {
		t.Fatal(*calls)
	}
	for _, uid := range []string{"a", "b"} {
		h.cfg.Pool.SetDailyTokenLimit(uid, pool.DeepseekPaidModel, 1)
		h.cfg.Pool.RecordTokenUsage(uid, pool.TokenUsageDelta{Model: pool.DeepseekPaidModel, HasTotalTokens: true, TotalTokens: 1})
	}
	r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, false)
	if r.Code != 429 || !strings.Contains(r.Body.String(), "daily_token_limit") || len(*calls) != 1 {
		t.Fatal(r.Code, r.Body, *calls)
	}
	h.cfg.Pool.SetDailyTokenLimit("b", pool.DeepseekFreeModel, 101)
	if r := fallbackRequest(h, "global:"+pool.DeepseekFreeModel, false); r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	if (*calls)[1].model != pool.DeepseekFreeModel {
		t.Fatal("restored free model not preferred")
	}
}
