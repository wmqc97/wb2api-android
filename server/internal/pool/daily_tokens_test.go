package pool

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func dailyPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	p.SetPickMode(PickSequential)
	p.Add(&auth.Auth{UID: "a", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "b", Domain: "www.workbuddy.ai"})
	p.SetCredits("a", 100, 1000)
	p.SetCredits("b", 100, 1000)
	p.SetOrder([]string{"a", "b"})
	return p
}
func TestDailyTokenLimitRoutingAndModelIsolation(t *testing.T) {
	p := dailyPool(t)
	if err := p.SetDailyTokenLimit("a", "global:model", 100); err != nil {
		t.Fatal(err)
	}
	p.RecordTokenUsage("a", TokenUsageDelta{Model: "model", HasPromptTokens: true, PromptTokens: 60, HasCompletionTokens: true, CompletionTokens: 40})
	if a := p.PickExcludingForRealm(nil, "model", "global"); a == nil || a.UID != "b" {
		t.Fatal("exhausted account selected")
	}
	if a := p.PickByUIDForModel("a", "model"); a != nil {
		t.Fatal("sticky bypassed limit")
	}
	if p.AcquireForModel("a", "global:model") {
		t.Fatal("acquire bypassed limit")
	}
	if a := p.PickByUIDForModel("a", "other"); a == nil {
		t.Fatal("unrelated model blocked")
	}
	p.Cooldown("a", CoolSoft, time.Hour, "test")
	if a := p.PickExcludingForRealm(map[string]bool{"b": true}, "model", "global"); a != nil {
		t.Fatal("cooling fallback bypassed limit")
	}
	p.Revive("a")
	if p.AcquireForModel("a", "model") {
		t.Fatal("revive reset quota")
	}
	if err := p.SetDailyTokenLimit("a", "model", 101); err != nil {
		t.Fatal(err)
	}
	if a := p.PickByUIDForModel("a", "model"); a == nil {
		t.Fatal("raised limit did not take effect")
	}
	p.SetDailyTokenLimit("a", "model", 0)
	st, _ := p.Status("a")
	if len(st.DailyTokenUsage) != 1 || st.DailyTokenUsage[0].Total != 100 {
		t.Fatal("changing limit cleared consumption")
	}
}
func TestDailyTokenMidnightAndPersistence(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	p := New(state)
	p.Add(&auth.Auth{UID: "a"})
	p.SetDailyTokenLimit("a", "m", 100)
	before := time.Date(2026, 10, 11, 15, 59, 59, 0, time.UTC)
	after := before.Add(time.Second)
	p.mu.Lock()
	p.byUID["a"].recordDailyTokens(before, TokenUsageDelta{Model: "m", HasTotalTokens: true, TotalTokens: 100})
	if !p.byUID["a"].dailyTokenBlocked(before, "m") || p.byUID["a"].dailyTokenBlocked(after, "m") {
		t.Fatal("Beijing midnight reset incorrect")
	}
	p.mu.Unlock()
	p.RecordTokenUsage("a", TokenUsageDelta{Model: "m", HasTotalTokens: true, TotalTokens: 100})
	p.Flush()
	p.Close()
	restored := New(state)
	defer restored.Close()
	restored.Add(&auth.Auth{UID: "a"})
	st, _ := restored.Status("a")
	if len(st.DailyTokenUsage) != 1 || st.DailyTokenUsage[0].Limit != 100 || !st.DailyTokenUsage[0].Limited {
		t.Fatalf("state lost after reload: %+v", st.DailyTokenUsage)
	}
}
func TestDailyTokenConcurrentAccountingAndSeed(t *testing.T) {
	p := dailyPool(t)
	now := time.Now()
	p.SeedDailyTokens(now, map[string]map[string]DailyModelTokens{"a": {"m": {Day: DailyTokenDay(now), Total: 50}}})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.RecordTokenUsage("a", TokenUsageDelta{Model: "global:m", HasTotalTokens: true, TotalTokens: 1})
		}()
	}
	wg.Wait()
	p.SeedDailyTokens(now, map[string]map[string]DailyModelTokens{"a": {"m": {Day: DailyTokenDay(now), Total: 80}}})
	st, _ := p.Status("a")
	if st.DailyTokenUsage[0].Total != 150 {
		t.Fatal(st.DailyTokenUsage)
	}
	st.DailyTokenUsage[0].Total = 0
	st, _ = p.Status("a")
	if st.DailyTokenUsage[0].Total != 150 {
		t.Fatal("status leaks mutable state")
	}
}
func TestDailyLimitFreePoolFallsBackToSG(t *testing.T) {
	p := dailyPool(t)
	for _, uid := range []string{"a", "b"} {
		p.SetDailyTokenLimit(uid, DeepseekFreeModel, 100)
		p.RecordTokenUsage(uid, TokenUsageDelta{Model: DeepseekFreeModel, HasTotalTokens: true, TotalTokens: 100})
	}
	if !p.DeepseekFreeExhausted() || p.PickDeepseekPaidFallback(nil) == nil {
		t.Fatal("local free quota did not allow SG fallback")
	}
	for _, uid := range []string{"a", "b"} {
		p.SetDailyTokenLimit(uid, DeepseekPaidModel, 100)
		p.RecordTokenUsage(uid, TokenUsageDelta{Model: DeepseekPaidModel, HasTotalTokens: true, TotalTokens: 100})
	}
	if p.PickDeepseekPaidFallback(nil) != nil {
		t.Fatal("SG ignored its own daily quota")
	}
	p.SetDailyTokenLimit("b", DeepseekFreeModel, 0)
	if p.DeepseekFreeExhausted() || p.PickDeepseekPaidFallback(nil) != nil {
		t.Fatal("restored free quota must take priority")
	}
}
