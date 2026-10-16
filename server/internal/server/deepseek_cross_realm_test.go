package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func mixedDeepseekFixture(t *testing.T) (*Handler, *[]fallbackCall) {
	t.Helper()
	h, calls := fallbackFixture(t, "global")
	h.cfg.Pool.Add(&auth.Auth{UID: "cn", Domain: "www.codebuddy.cn", AccessToken: "cn", ExpiresAt: 9999999999})
	h.cfg.Pool.SetCredits("cn", 100, 1000)
	h.cfg.Upstream.GlobalEnabled = true
	h.cfg.Upstream.ChatBaseCN = "https://fake-cn.example"
	chatTransport := h.cfg.Upstream.HTTP.Transport
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return chatTransport.RoundTrip(r)
		}
		catalog := `{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash"},{"id":"deepseek-v4.1-flash-sg"}]}}`
		if r.Header.Get("Authorization") == "Bearer cn" {
			catalog = `{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash"}]}}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(catalog))}, nil
	})
	for _, uid := range []string{"cn", "global"} {
		if _, err := h.cfg.Upstream.FetchModels(h.cfg.Pool.AuthByUID(uid)); err != nil {
			t.Fatal(err)
		}
	}
	return h, calls
}

func TestBareDeepseekInternationalThenSGThenCN(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			h, calls := mixedDeepseekFixture(t)
			if r := fallbackRequest(h, pool.DeepseekFreeModel, stream); r.Code != 200 || (*calls)[0] != (fallbackCall{pool.DeepseekFreeModel, "Bearer global"}) {
				t.Fatalf("international free: status=%d calls=%v body=%s", r.Code, *calls, r.Body.String())
			}
			exhaustFree(h.cfg.Pool, "global")
			if r := fallbackRequest(h, pool.DeepseekFreeModel, stream); r.Code != 200 || (*calls)[1] != (fallbackCall{pool.DeepseekPaidModel, "Bearer global"}) {
				t.Fatalf("SG must precede paid CN: status=%d calls=%v body=%s", r.Code, *calls, r.Body.String())
			}
			h.cfg.Pool.CooldownSoftForModel("global", time.Hour, time.Now().Add(time.Hour), pool.DeepseekPaidModel, "6004 model rate limit")
			r := fallbackRequest(h, pool.DeepseekFreeModel, stream)
			if r.Code != 200 || len(*calls) != 3 || (*calls)[2] != (fallbackCall{pool.DeepseekFreeModel, "Bearer cn"}) {
				t.Fatalf("CN skipped after SG exhaustion: status=%d calls=%v body=%s", r.Code, *calls, r.Body.String())
			}
			if r.Header().Get("X-WB2API-Actual-Model") != "cn:"+pool.DeepseekFreeModel {
				t.Fatalf("incorrect actual route: %v", r.Header())
			}
			found := false
			for _, row := range h.cfg.Usage.Snapshot(24, nil).ByModel {
				if row.Key == "cn:"+pool.DeepseekFreeModel && row.Requests == 1 {
					found = true
				}
			}
			if !found {
				t.Fatal("CN usage attributed to international/SG model")
			}
		})
	}
}

func TestBareDeepseekLastSGAttemptCanEnterCN(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			h, calls := mixedDeepseekFixture(t) // max_rotate=1 for each phase
			base := h.cfg.Upstream.HTTP.Transport
			h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost && r.Header.Get("Authorization") == "Bearer global" {
					var b map[string]any
					json.NewDecoder(r.Body).Decode(&b)
					*calls = append(*calls, fallbackCall{b["model"].(string), "Bearer global"})
					until := time.Now().Add(time.Hour).In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
					return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":6004,"msg":"将在 ` + until + ` UTC+8 重置"}`))}, nil
				}
				return base.RoundTrip(r)
			})
			r := fallbackRequest(h, pool.DeepseekFreeModel, stream)
			want := []fallbackCall{{pool.DeepseekFreeModel, "Bearer global"}, {pool.DeepseekPaidModel, "Bearer global"}, {pool.DeepseekFreeModel, "Bearer cn"}}
			if r.Code != 200 || len(*calls) != len(want) {
				t.Fatalf("bounded three-phase fallback: status=%d calls=%v body=%s", r.Code, *calls, r.Body.String())
			}
			for i := range want {
				if (*calls)[i] != want[i] {
					t.Fatalf("incorrect fallback order: %v", *calls)
				}
			}
		})
	}
}

func TestBareDeepseekCNPaidReserveAndDailyQuota(t *testing.T) {
	for _, state := range []string{"above_reserve", "at_reserve", "unknown_balance", "daily_limit"} {
		t.Run(state, func(t *testing.T) {
			h, calls := mixedDeepseekFixture(t)
			exhaustFree(h.cfg.Pool, "global")
			h.cfg.Pool.CooldownSoftForModel("global", time.Hour, time.Now().Add(time.Hour), pool.DeepseekPaidModel, "6004 model rate limit")
			h.cfg.Pool.SetFreeModelLookup(func(string) bool { return true }) // CN's identical ID must not inherit the international free exemption.
			want := 503
			switch state {
			case "above_reserve":
				want = 200
			case "at_reserve":
				h.cfg.Pool.SetCredits("cn", 50, 1000)
			case "unknown_balance":
				h.cfg.Pool.SetCredits("cn", 100, 0)
			case "daily_limit":
				want = 429
				h.cfg.Pool.SetDailyTokenLimit("cn", pool.DeepseekFreeModel, 1)
				h.cfg.Pool.RecordTokenUsage("cn", pool.TokenUsageDelta{Model: pool.DeepseekFreeModel, HasTotalTokens: true, TotalTokens: 1})
			}
			r := fallbackRequest(h, pool.DeepseekFreeModel, false)
			if r.Code != want || (len(*calls) > 0) != (want == 200) {
				t.Fatalf("state=%s status=%d calls=%v body=%s", state, r.Code, *calls, r.Body.String())
			}
			if state == "daily_limit" && (!strings.Contains(r.Body.String(), "daily_token_limit") || r.Header().Get("Retry-After") == "") {
				t.Fatalf("CN quota not reported: %s", r.Body.String())
			}
		})
	}
}

func TestBareDeepseekCNAfterSGReserveReached(t *testing.T) {
	h, calls := mixedDeepseekFixture(t)
	exhaustFree(h.cfg.Pool, "global")
	h.cfg.Pool.SetCredits("global", 50, 1000)
	r := fallbackRequest(h, pool.DeepseekFreeModel, false)
	if r.Code != 200 || len(*calls) != 1 || (*calls)[0] != (fallbackCall{pool.DeepseekFreeModel, "Bearer cn"}) {
		t.Fatalf("SG at reserve should yield to CN: status=%d calls=%v body=%s", r.Code, *calls, r.Body.String())
	}
}

func TestDeepseekExplicitRegionAndSGRemainSeparate(t *testing.T) {
	for _, tc := range []struct {
		model      string
		wantStatus int
		wantToken  string
	}{
		{"global:" + pool.DeepseekFreeModel, 429, ""},
		{"cn:" + pool.DeepseekFreeModel, 200, "Bearer cn"},
		{pool.DeepseekPaidModel, 429, ""},
	} {
		t.Run(tc.model, func(t *testing.T) {
			h, calls := mixedDeepseekFixture(t)
			exhaustFree(h.cfg.Pool, "global")
			h.cfg.Pool.CooldownSoftForModel("global", time.Hour, time.Now().Add(time.Hour), pool.DeepseekPaidModel, "6004 model rate limit")
			r := fallbackRequest(h, tc.model, false)
			if r.Code != tc.wantStatus || (len(*calls) > 0) != (tc.wantToken != "") {
				t.Fatalf("explicit/SG route changed: status=%d calls=%v body=%s", r.Code, *calls, r.Body.String())
			}
			if tc.wantToken != "" && (*calls)[0].token != tc.wantToken {
				t.Fatal(*calls)
			}
		})
	}
}

func TestBareDeepseekCNDoesNotStickAfterInternationalRecovery(t *testing.T) {
	h, calls := mixedDeepseekFixture(t)
	h.cfg.Session = session.New(session.Config{AvailableForModel: func(string) []string { return []string{"global", "cn"} }})
	exhaustFree(h.cfg.Pool, "global")
	h.cfg.Pool.CooldownSoftForModel("global", time.Hour, time.Now().Add(time.Hour), pool.DeepseekPaidModel, "6004 model rate limit")
	request := func() {
		raw := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"cn-recovery"}}`
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(raw)))
		if r.Code != 200 {
			t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
		}
	}
	request()
	h.cfg.Pool.ClearModelCooldown("global", pool.DeepseekPaidModel)
	request()
	h.cfg.Pool.ClearModelCooldown("global", pool.DeepseekFreeModel)
	request()
	want := []fallbackCall{{pool.DeepseekFreeModel, "Bearer cn"}, {pool.DeepseekPaidModel, "Bearer global"}, {pool.DeepseekFreeModel, "Bearer global"}}
	if len(*calls) != len(want) {
		t.Fatal(*calls)
	}
	for i := range want {
		if (*calls)[i] != want[i] {
			t.Fatalf("sticky CN bypassed cheaper recovery: %v", *calls)
		}
	}
}

func TestBareDeepseekSpecificChainCanExitGlobalCatalogGroup(t *testing.T) {
	h, calls := mixedDeepseekFixture(t)
	// v1.9.40 can classify 4.1 as global-only after a directory omission. This
	// opt-in chain is specific to DeepSeek; Kimi's exclusive grouping stays intact.
	h.router.ModelRealms = func(string) (bool, bool) { return false, true }
	exhaustFree(h.cfg.Pool, "global")
	h.cfg.Pool.CooldownSoftForModel("global", time.Hour, time.Now().Add(time.Hour), pool.DeepseekPaidModel, "6004 model rate limit")
	r := fallbackRequest(h, pool.DeepseekFreeModel, false)
	if r.Code != 200 || len(*calls) != 1 || (*calls)[0].token != "Bearer cn" {
		t.Fatalf("global catalog group trapped bare 4.1: status=%d calls=%v body=%s", r.Code, *calls, r.Body.String())
	}
}
