package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func catalogRealmHandler(t *testing.T, precedence string) (*Handler, *pool.Pool, *[]realmCall) {
	t.Helper()
	p := realmMixedPool(t)
	up, calls := newRealmUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	chatTransport := up.HTTP.Transport
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			return chatTransport.RoundTrip(r)
		}
		ids := []string{"kimi-k3.1", "shared"}
		if r.Header.Get("Authorization") == "Bearer at-global" {
			ids = []string{"global-only", "shared"}
		}
		models := make([]upstream.ModelInfo, 0, len(ids))
		for _, id := range ids {
			models = append(models, upstream.ModelInfo{ID: id})
		}
		raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"models": models}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})
	for _, uid := range []string{"c28ceb89", "bebb8506"} {
		if _, err := up.FetchModels(p.AuthByUID(uid)); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true, RealmPrecedence: precedence, StripRealmPrefix: true})
	return h, p, calls
}

func catalogChat(t *testing.T, h *Handler, model string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"model": model, "stream": stream, "messages": []any{}, "metadata": map[string]string{"conversation_id": "group-test"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw))))
	return rec
}

func TestChatCatalogRoutesBeforeRealmPrecedence(t *testing.T) {
	for _, tc := range []struct{ precedence, model, want string }{
		{"global", "kimi-k3.1", "Bearer at-cn"},
		{"cn", "global-only", "Bearer at-global"},
		{"global", "shared", "Bearer at-global"},
		{"cn", "shared", "Bearer at-cn"},
		{"global", "unknown", "Bearer at-global"},
		{"global", "global:kimi-k3.1", "Bearer at-global"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.precedence+"/"+tc.model+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				h, _, calls := catalogRealmHandler(t, tc.precedence)
				rec := catalogChat(t, h, tc.model, stream)
				if rec.Code != 200 || len(*calls) != 1 || (*calls)[0].auth != tc.want {
					t.Fatalf("status=%d calls=%+v body=%s", rec.Code, *calls, rec.Body.String())
				}
			})
		}
	}
}

func TestChatCatalogNeverFallsBackToWrongRealm(t *testing.T) {
	h, p, calls := catalogRealmHandler(t, "global")
	p.CooldownSoftForModel("c28ceb89", time.Hour, time.Now().Add(time.Hour), "kimi-k3.1", "6004 model rate limit")
	rec := catalogChat(t, h, "kimi-k3.1", false)
	if rec.Code != http.StatusTooManyRequests || len(*calls) != 0 {
		t.Fatalf("exhausted CN pool must not send Kimi to global: code=%d calls=%+v", rec.Code, *calls)
	}
	p.ClearModelCooldown("c28ceb89", "kimi-k3.1")
	// A previously wrong global account is now fully healthy again. Grouping
	// must still prevent selecting it after account/model cooldown state clears.
	p.BlockModelBackoff("bebb8506", "kimi-k3.1", "11102 service info not found")
	p.ClearModelCooldown("bebb8506", "kimi-k3.1")
	for i := 0; i < 3; i++ {
		if rec := catalogChat(t, h, "kimi-k3.1", false); rec.Code != 200 {
			t.Fatalf("CN restored: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, call := range *calls {
		if call.auth != "Bearer at-cn" {
			t.Fatalf("cooldown reset lost realm membership: %+v", *calls)
		}
	}
}

func TestChatCatalogRejectsOldStickyBindingToWrongRealm(t *testing.T) {
	h, p, calls := catalogRealmHandler(t, "global")
	h.cfg.Session = session.New(session.Config{AvailableForModel: func(string) []string { return []string{"bebb8506", "c28ceb89"} }})
	body := []byte(`{"metadata":{"conversation_id":"group-test"}}`)
	h.cfg.Session.Bind(session.ExtractKey(body), "bebb8506")
	if rec := catalogChat(t, h, "kimi-k3.1", false); rec.Code != 200 {
		t.Fatalf("sticky catalog route: %d %s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 1 || (*calls)[0].auth != "Bearer at-cn" {
		t.Fatalf("old sticky binding leaked to global: %+v", *calls)
	}
	p.Disable("c28ceb89", "test disabled")
	before := len(*calls)
	catalogChat(t, h, "kimi-k3.1", false)
	if len(*calls) != before {
		t.Fatal("disabled CN group fell back to unsupported global")
	}
}

func TestCatalogRouterPreservesGlobalGateAndExplicitPrefixes(t *testing.T) {
	r := RealmRouter{GlobalEnabled: true, Precedence: "global", HasRealm: func(string) bool { return true }, ModelRealms: func(model string) (bool, bool) {
		return model == "cn-only", model == "global-only"
	}}
	if realm, _ := r.Resolve("cn-only"); realm != "cn" {
		t.Fatal("CN catalog owner was overridden by global precedence")
	}
	if realm, _ := r.Resolve("global:cn-only"); realm != "global" {
		t.Fatal("explicit global prefix lost priority")
	}
	r.GlobalEnabled = false
	if realm, _ := r.Resolve("global-only"); realm != "cn" {
		t.Fatal("catalog grouping bypassed global.enabled=false")
	}
}
