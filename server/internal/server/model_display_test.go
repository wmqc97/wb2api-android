package server

import "testing"

// TestModelDisplayName 外部调用的模型名带倍率（参考实现 format_entries 口径）。
func TestModelDisplayName(t *testing.T) {
	cases := []struct{ id, credits, want string }{
		{"deepseek-v4.1-flash", "x0.25", "deepseek-v4.1-flash · ×0.25"},
		{"glm-5.2", "X0.79", "glm-5.2 · ×0.79"},
		{"hy4-preview", "0.03", "hy4-preview · ×0.03"},
		{"no-credits", "", "no-credits"}, // 上游没给倍率 → 不显示倍率
		{"only-x", "x", "only-x"},        // 异常值 → 裸名
		{"  padded  ", " x1.00 ", "padded · ×1.00"},
	}
	// 图片能力标识（用户要求：列表里要看得出哪些能处理图片）
	if got := modelDisplayName("glm-5.2", "x0.79", true); got != "glm-5.2 · ×0.79 · 可传图" {
		t.Errorf("带图片能力 name=%q", got)
	}
	if got := modelDisplayName("glm-5.2", "x0.79", false); got != "glm-5.2 · ×0.79" {
		t.Errorf("无图片能力 name=%q", got)
	}
	for _, c := range cases {
		if got := modelDisplayName(c.id, c.credits, false); got != c.want {
			t.Errorf("modelDisplayName(%q,%q)=%q want %q", c.id, c.credits, got, c.want)
		}
	}
}
