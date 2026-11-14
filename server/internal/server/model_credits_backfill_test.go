package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestFillModelCredits 上游这轮没给倍率时，用历史旁表回填，name 才能带上「×倍率」。
//
// 线上症状（用户报「只有新的模型有倍率，之前的没显示」）：CN 目录里同一模型在不同
// 账号/不同探测轮次里 credits 时有时无，缺了就只剩裸名，客户端看不出花多少积分。
func TestFillModelCredits(t *testing.T) {
	upstream.RecordModelCredits("cn", []upstream.ModelInfo{
		{ID: "glm-5.2", Credits: "x0.79"},
	})

	infos := fillModelCredits("cn", []upstream.ModelInfo{
		{ID: "glm-5.2"},                   // 缺倍率 → 回填 x0.79
		{ID: "gpt-5.4", Credits: "x1.65"}, // 上游已给 → 保留
		{ID: "unknown-model"},             // 从未观测 → 无倍率
	})
	if infos[0].Credits != "x0.79" {
		t.Errorf("glm-5.2 credits=%q want x0.79", infos[0].Credits)
	}
	if infos[1].Credits != "x1.65" {
		t.Errorf("上游值被覆盖了：%q", infos[1].Credits)
	}
	if infos[2].Credits != "" {
		t.Errorf("未观测模型不该凭空有倍率：%q", infos[2].Credits)
	}
	// /v1/models 的展示名口径
	want := "glm-5.2 · ×0.79"
	if got := modelDisplayName(infos[0].ID, infos[0].Credits, false); got != want {
		t.Errorf("name=%q want %q", got, want)
	}
	// 旁表按 realm 隔离：global 域不该命中 CN 的观测
	if _, ok := upstream.ModelCreditsFor("global", "glm-5.2"); ok {
		t.Error("global 域不应命中 CN 倍率（同 id 两域不同价）")
	}
}
