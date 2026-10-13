package usage

import (
	"testing"
	"time"
)

func TestDailyAccountTokensBeijingBoundaryAndAliases(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)
	r.Add(time.Date(2026, 10, 11, 15, 59, 0, 0, time.UTC), "global", "a", "m", Delta{TotalTokens: 999, HasTotal: true}, true)
	r.Add(time.Date(2026, 10, 11, 16, 1, 0, 0, time.UTC), "global", "a", "global:m", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true, TotalTokens: 15, HasTotal: true}, true)
	r.Add(time.Date(2026, 10, 11, 17, 1, 0, 0, time.UTC), "global", "a", "m", Delta{TotalTokens: 25, HasTotal: true}, true)
	totals := r.DailyAccountTokens(now)
	u := totals["a"]["m"]
	if len(totals["a"]) != 1 || u.Day != "2026-10-12" || u.Total != 40 || u.Input != 10 || u.Output != 5 {
		t.Fatalf("unexpected Beijing daily totals: %+v", totals)
	}
}
