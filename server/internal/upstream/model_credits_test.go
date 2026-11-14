package upstream

import "testing"

// TestModelCreditsBackfillTable 倍率旁表：按 realm 隔离、空值不覆盖旧值。
func TestModelCreditsBackfillTable(t *testing.T) {
	resetModelCreditsForTest()
	RecordModelCredits("cn", []ModelInfo{
		{ID: "glm-5.2", Credits: "x0.79"},
		{ID: "no-credit"},
		{ID: "", Credits: "x9.99"},
	})
	if c, ok := ModelCreditsFor("cn", "glm-5.2"); !ok || c != "x0.79" {
		t.Fatalf("cn glm-5.2 = %q,%v want x0.79", c, ok)
	}
	if _, ok := ModelCreditsFor("cn", "no-credit"); ok {
		t.Error("空倍率不应入表")
	}
	if _, ok := ModelCreditsFor("global", "glm-5.2"); ok {
		t.Error("跨域不应命中：同 id 在两域是不同价")
	}
	// 后续一轮没给倍率：不得把已观测到的值冲掉
	RecordModelCredits("cn", []ModelInfo{{ID: "glm-5.2"}})
	if c, ok := ModelCreditsFor("cn", "glm-5.2"); !ok || c != "x0.79" {
		t.Fatalf("空值覆盖了旧倍率：%q,%v", c, ok)
	}
	resetModelCreditsForTest()
}
