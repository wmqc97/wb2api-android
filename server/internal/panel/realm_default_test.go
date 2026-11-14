package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestPanelDefaultRealmFollowsUsableAccounts 面板默认域要跟着「现在能干活的账号」走。
//
// 线上症状（用户报「网页管理端的模型能力列表不能获取」）：两个 global 号余额烧光
// 且被手动停用，默认域仍判成 global → 「模型与档位」一打开就报
// 「没有 global 域的账号」，而池里其实有健康的 CN 号。
func TestPanelDefaultRealmFollowsUsableAccounts(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	pn := New(Config{APIKey: "", Pool: p})

	if got := pn.defaultRealm(); got != "global" {
		t.Fatalf("两域都有可用账号时应沿用 global，got %q", got)
	}
	// global 号被停用 → 默认域回落 cn
	p.SetManualDisabled("g1", true, "余额耗尽")
	if got := pn.defaultRealm(); got != "cn" {
		t.Fatalf("global 全停用时应回落 cn，got %q", got)
	}
	// 恢复后回到 global
	p.SetManualDisabled("g1", false, "")
	if got := pn.defaultRealm(); got != "global" {
		t.Fatalf("恢复后应回到 global，got %q", got)
	}
}

// TestPoolHasUsableRealm HasUsableRealm 与 HasRealm 的分工：前者只数「能干活的」。
func TestPoolHasUsableRealm(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	if !p.HasRealm("global") || !p.HasUsableRealm("global") {
		t.Fatal("新建的 global 号应同时满足 HasRealm/HasUsableRealm")
	}
	p.Disable("g1", "人工禁用")
	if !p.HasRealm("global") {
		t.Error("禁用后 HasRealm 仍应 true（账号还在池里）")
	}
	if p.HasUsableRealm("global") {
		t.Error("禁用后 HasUsableRealm 应为 false")
	}
}
