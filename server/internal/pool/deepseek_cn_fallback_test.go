package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func cnDeepseekPool(t *testing.T) *Pool {
	t.Helper()
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(prev) })
	p := New("")
	p.SetReserveCredits(50)
	p.Add(&auth.Auth{UID: "global", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "cn", Domain: "www.codebuddy.cn"})
	p.SetCredits("global", 100, 1000)
	p.SetCredits("cn", 100, 1000)
	p.CooldownSoftForModel("global", time.Hour, time.Now().Add(time.Hour), DeepseekFreeModel, "6004 model rate limit")
	return p
}

func TestDeepseekCNFallbackRechecksInternationalRecovery(t *testing.T) {
	p := cnDeepseekPool(t)
	if p.DeepseekCNFallbackReady() || p.PickDeepseekCNFallback(nil) != nil {
		t.Fatal("CN selected while SG still has quota")
	}
	p.CooldownSoftForModel("global", time.Hour, time.Now().Add(time.Hour), DeepseekPaidModel, "6004 model rate limit")
	if !p.DeepseekCNFallbackReady() {
		t.Fatal("CN phase not ready after international exhaustion")
	}
	if a := p.PickDeepseekCNFallback(nil); a == nil || a.UID != "cn" {
		t.Fatal("CN candidate not selected")
	}
	p.ClearModelCooldown("global", DeepseekPaidModel)
	if p.PickDeepseekCNFallback(nil) != nil {
		t.Fatal("selection did not recheck SG quota restored after readiness check")
	}
	p.CooldownSoftForModel("global", time.Hour, time.Now().Add(time.Hour), DeepseekPaidModel, "6004 model rate limit")
	p.ClearModelCooldown("global", DeepseekFreeModel)
	if p.PickDeepseekCNFallback(nil) != nil {
		t.Fatal("CN selected after international free quota recovered")
	}
}

func TestDeepseekCNFallbackRequiresQuotaOrReserveExhaustion(t *testing.T) {
	for _, state := range []string{"busy", "network", "missing", "unknown_balance", "reserve", "daily_limit"} {
		t.Run(state, func(t *testing.T) {
			p := cnDeepseekPool(t)
			want := false
			switch state {
			case "busy":
				p.SetMaxInFlightGlobal(1)
				p.Acquire("global")
			case "network":
				p.Cooldown("global", CoolSoft, time.Hour, "WAF")
			case "missing":
				p.BlockModelBackoff("global", DeepseekPaidModel, "11102 service info not found")
			case "unknown_balance":
				p.SetCredits("global", 0, 0)
			case "reserve":
				p.SetCredits("global", 50, 1000)
				want = true
			case "daily_limit":
				p.SetDailyTokenLimit("global", DeepseekPaidModel, 1)
				p.RecordTokenUsage("global", TokenUsageDelta{Model: DeepseekPaidModel, HasTotalTokens: true, TotalTokens: 1})
				want = true
			}
			if got := p.DeepseekCNFallbackReady(); got != want {
				t.Fatalf("state=%s readiness=%v want=%v", state, got, want)
			}
			if a := p.PickDeepseekCNFallback(nil); (a != nil) != want {
				t.Fatalf("state=%s picked=%v want=%v", state, a, want)
			}
		})
	}
}

func TestDeepseekCNFallbackWaitsForAllInternationalAccounts(t *testing.T) {
	p := cnDeepseekPool(t)
	p.Add(&auth.Auth{UID: "second-global", Domain: "www.workbuddy.ai"})
	p.SetCredits("second-global", 100, 1000)
	p.CooldownSoftForModel("second-global", time.Hour, time.Now().Add(time.Hour), DeepseekFreeModel, "6004 model rate limit")
	p.CooldownSoftForModel("global", time.Hour, time.Now().Add(time.Hour), DeepseekPaidModel, "6004 model rate limit")
	if p.DeepseekCNFallbackReady() || p.PickDeepseekCNFallback(nil) != nil {
		t.Fatal("CN bypassed a second international account with SG quota")
	}
	p.SetCredits("second-global", 50, 1000)
	if !p.DeepseekCNFallbackReady() || p.PickDeepseekCNFallback(nil) == nil {
		t.Fatal("mixed SG quota/reserve exhaustion did not enable CN")
	}
	p.SetCredits("second-global", 51, 1000)
	if p.PickDeepseekCNFallback(nil) != nil {
		t.Fatal("SG balance recovery did not restore cheaper priority")
	}
}
