package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestPanelModelsFallsBackToCooledAccount 「模型与档位」在账号烧光余额时仍要能打开。
//
// 线上症状（用户报「获取不到模型列表」）：池里两个 global 号 credits=0 全部硬冷却，
// panel.models 用 PickExcludingForRealmMeta 选不到号 → 503「没有可用的 global 账号」。
// 而模型目录是只读接口、不扣积分，0 余额的号照样能拉 —— 修法是选号失败时用
// Pool.PickForCatalog 放宽到「该域任意未禁用账号（含冷却号）」。
func TestPanelModelsFallsBackToCooledAccount(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var v2Hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/enterprises/personal/models" {
			v2Hits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM 5.2","maxInputTokens":131072,"maxOutputTokens":32768,"credits":"x0.79"}
			]}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "tok"})
	p.SetCredits("g1", 0, 0)
	p.Cooldown("g1", pool.CoolHard, time.Hour, "余额耗尽")

	up := upstream.New()
	up.ChatBaseGlobal = srv.URL
	up.BillingBaseGlobal = srv.URL

	pn := New(Config{
		Version: "test", APIKey: "test-key", Pool: p, Upstream: up,
		HiddenModels: upstream.ResolveHiddenModels(nil),
		PinnedModels: upstream.ResolvePinnedModels(nil),
	})
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（余额烧光的号也应能拉目录）", rec.Code, rec.Body.String())
	}
	if v2Hits == 0 {
		t.Fatal("未请求 global 目录端点：兜底选号没生效")
	}
}
