package main

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
)

func TestStickyCatalogGroupingOverridesPrecedence(t *testing.T) {
	previous := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(previous) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "global", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "cn", Domain: "www.codebuddy.cn"})
	cfg := Default()
	cfg.Models.RealmPrecedence = "global"
	r := newSessionRouter(cfg, p, redisstore.Noop{}, func(model string) (bool, bool) {
		return model == "kimi-k3.1", model == "global-only"
	})
	r.Bind("old-session", "global")
	if uid, ok := r.ResolveForModel("old-session", "kimi-k3.1"); !ok || uid != "cn" {
		t.Fatalf("sticky route for CN-only Kimi: uid=%s ok=%v", uid, ok)
	}
	if uid, ok := r.ResolveForModel("new-session", "global-only"); !ok || uid != "global" {
		t.Fatalf("sticky route for global-only model: uid=%s ok=%v", uid, ok)
	}
}
