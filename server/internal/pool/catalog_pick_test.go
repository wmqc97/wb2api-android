package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestPickForCatalogAllowsCooledAccount 元数据选号必须能借到「余额烧光」的号。
//
// 线上症状：只有 global 账号、且两个号都烧光（credits=0 → CoolHard 硬冷却）时，
// 面板「模型与档位」直接报「没有可用的 global 账号」。模型目录是只读接口、不消费
// 积分，0 余额的号照样能返回目录 —— 用 chat 的选号谓词把这类号排除，用户正好在
// 最需要看清「还剩什么模型、多少钱」的时刻看不到任何东西。
func TestPickForCatalogAllowsCooledAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.SetCredits("g1", 0, 0)
	p.Cooldown("g1", CoolHard, time.Hour, "余额耗尽")

	// 前置：chat 口径确实借不到（复现线上的 nil）
	if a := p.PickExcludingForRealmMeta(nil, "global"); a != nil {
		t.Fatalf("前置条件不成立：硬冷却账号不该被 meta 选号命中（got %s）", a.UID)
	}
	// 修复后：元数据专用入口能借到
	a := p.PickForCatalog("global")
	if a == nil {
		t.Fatal("PickForCatalog 返回 nil：0 余额的号也应能用来拉模型目录")
	}
	if a.UID != "g1" {
		t.Errorf("PickForCatalog=%s want g1", a.UID)
	}
}

// TestPickForCatalogSkipsDisabled 禁用/临时停用的账号永不被展示路径绕过。
func TestPickForCatalogSkipsDisabled(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "off", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "susp", Domain: "www.workbuddy.ai"})
	p.Disable("off", "人工禁用")
	p.SetManualDisabled("susp", true, "临时停用")
	if a := p.PickForCatalog("global"); a != nil {
		t.Fatalf("禁用/停用账号被选中：%s", a.UID)
	}
}

// TestPickForCatalogRealmScoped 只借同域的号：跨域会拉到另一个域的目录（串域）。
func TestPickForCatalogRealmScoped(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "cn1"})
	p.SetCredits("cn1", 100, 100)
	if a := p.PickForCatalog("global"); a != nil {
		t.Fatalf("global 域不该借到 cn 号：%s", a.UID)
	}
	if a := p.PickForCatalog("cn"); a == nil || a.UID != "cn1" {
		t.Fatalf("cn 域应命中 cn1，got %v", a)
	}
}
