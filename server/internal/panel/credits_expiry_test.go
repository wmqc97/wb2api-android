package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestCreditsExpiryEndpoint 「积分到期」端点：账号级汇总 + 逐包明细，按「最先作废」排序。
//
// 锁定的口径（与参考实现 admin/credits.py 一致）：
//   - 只有**还有剩余**的包参与「最早到期」与「窗口内剩余」统计；
//   - 窗口内到期的账号 rank=0 且排在最前；
//   - 已过期但仍有剩余的额度单独计入 expired_remaining，不计入可抢救额度。
func TestCreditsExpiryEndpoint(t *testing.T) {
	soon := cstWall(time.Now().Add(72 * time.Hour))     // 3 天后到期 → 快过期
	far := cstWall(time.Now().Add(30 * 24 * time.Hour)) // 30 天后到期 → 不紧急
	dead := cstWall(time.Now().Add(-24 * time.Hour))    // 已过期
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/get-user-resource") {
			http.Error(w, "not found", 404)
			return
		}
		// 按 Authorization 区分账号：u1 = 快到期包，u2 = 远期包 + 已过期包。
		acct := ""
		if r.Header.Get("Authorization") == "Bearer at-1" {
			acct = `{"PackageName":"国内运营裂变包","CycleEndTime":"` + soon + `","CapacitySize":1500,"CapacityRemain":900,"CapacityUsed":600},` +
				`{"PackageName":"长期包","CycleEndTime":"` + far + `","CapacitySize":200,"CapacityRemain":200,"CapacityUsed":0}`
		} else {
			acct = `{"PackageName":"拉新权益包","CycleEndTime":"` + far + `","CapacitySize":300,"CapacityRemain":300,"CapacityUsed":0},` +
				`{"PackageName":"过期包","CycleEndTime":"` + dead + `","CapacitySize":100,"CapacityRemain":40,"CapacityUsed":60}`
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[` + acct + `]}}}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at-2", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up, ExpiringSoonWindow: 7 * 24 * time.Hour})
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up, Scheduler: sch})

	req := httptest.NewRequest("GET", "/panel/api/credits/expiry", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		WindowDays int `json:"window_days"`
		Summary    struct {
			Accounts          int   `json:"accounts"`
			ExpiringAccounts  int   `json:"expiring_accounts"`
			ExpiringRemaining int64 `json:"expiring_soon_remaining"`
			ExpiredRemaining  int64 `json:"expired_remaining"`
			Packages          int   `json:"packages"`
		} `json:"summary"`
		Accounts []struct {
			UID          string `json:"uid"`
			Rank         int    `json:"rank"`
			ExpiringSoon int64  `json:"expiring_soon"`
			Expired      int64  `json:"expired_remaining"`
			Active       int    `json:"active_package_count"`
		} `json:"accounts"`
		Packages []struct {
			UID          string `json:"uid"`
			Name         string `json:"name"`
			Remain       int64  `json:"remain"`
			ExpiringSoon bool   `json:"expiring_soon"`
			Expired      bool   `json:"expired"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v body=%s", err, rec.Body.String())
	}
	if got.WindowDays != 7 {
		t.Errorf("window_days=%d want 7", got.WindowDays)
	}
	if got.Summary.Accounts != 2 || got.Summary.ExpiringAccounts != 1 {
		t.Errorf("summary=%+v", got.Summary)
	}
	if got.Summary.ExpiringRemaining != 900 {
		t.Errorf("expiring_soon_remaining=%d want 900（只有 u1 的 3 天包算快过期）", got.Summary.ExpiringRemaining)
	}
	if got.Summary.ExpiredRemaining != 40 {
		t.Errorf("expired_remaining=%d want 40（u2 的过期包剩余）", got.Summary.ExpiredRemaining)
	}
	if len(got.Accounts) != 2 || got.Accounts[0].UID != "u1" || got.Accounts[0].Rank != 0 {
		t.Fatalf("账号排行首行应为 u1(rank 0)：%+v", got.Accounts)
	}
	if got.Accounts[0].ExpiringSoon != 900 {
		t.Errorf("u1 expiring_soon=%d want 900", got.Accounts[0].ExpiringSoon)
	}
	// 包列表：快到期包必须排第一（按到期时间升序）；已过期的包也要在列（有剩余）。
	if len(got.Packages) < 3 {
		t.Fatalf("packages=%d（应含 u1 两个活包 + u2 一个到期包）", len(got.Packages))
	}
	if !got.Packages[0].Expired || got.Packages[0].UID != "u2" || got.Packages[0].Remain != 40 {
		t.Errorf("已过期包应排最前（到期时间最早）：%+v", got.Packages[0])
	}
	var sawSoon bool
	for _, it := range got.Packages {
		if it.UID == "u1" && it.Name == "国内运营裂变包" && it.ExpiringSoon {
			sawSoon = true
		}
	}
	if !sawSoon {
		t.Errorf("u1 的 3 天包应标记 expiring_soon：%+v", got.Packages)
	}
}

// TestCreditsBadge 侧栏角标走池内快过架子集（不打上游）：只看「有快过期剩余且未禁用」的账号。
func TestCreditsBadge(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 500, 500)
	p.SetCredits("b", 500, 500)
	p.SetCreditsExpiringAt("a", 120, time.Now().Add(48*time.Hour))
	pn := New(Config{Version: "test", APIKey: "", Pool: p})

	req := httptest.NewRequest("GET", "/panel/api/credits/badge", nil)
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var got struct {
		ExpiringAccounts int `json:"expiring_accounts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.ExpiringAccounts != 1 {
		t.Errorf("expiring_accounts=%d want 1 body=%s", got.ExpiringAccounts, rec.Body.String())
	}
}
