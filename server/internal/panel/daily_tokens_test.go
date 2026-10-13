package panel

import (
	"encoding/json"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDailyLimitParsing(t *testing.T) {
	for text, want := range map[string]int64{`"100M"`: 100000000, `"1.5M"`: 1500000, `"500k"`: 500000, `100000000`: 100000000, `"0"`: 0, `"2B"`: 2000000000} {
		n, e := parseTokenLimit(json.RawMessage(text))
		if e != nil || n != want {
			t.Fatal(text, n, e)
		}
	}
	for _, text := range []string{`null`, `-1`, `"NaN"`, `"Infinity"`, `"1e8"`, `"0.1"`, `"10000000000000000000000M"`, `{}`} {
		if _, e := parseTokenLimit(json.RawMessage(text)); e == nil {
			t.Fatal("accepted", text)
		}
	}
}
func TestDailyLimitPanelAuthAndLiveUpdate(t *testing.T) {
	pn, _ := newImportPanel(t)
	pn.cfg.Pool.Add(&auth.Auth{UID: "a", Domain: "www.workbuddy.ai"})
	pn.cfg.Pool.RecordTokenUsage("a", pool.TokenUsageDelta{Model: "m", HasTotalTokens: true, TotalTokens: 100000000})
	post := func(key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/panel/api/accounts/a/daily_tokens", strings.NewReader(body))
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		pn.ServeHTTP(w, r)
		return w
	}
	if w := post("", `{"model":"m","limit":"100M"}`); w.Code != 401 {
		t.Fatal("auth bypass", w.Code)
	}
	if w := post("test-key", `{"model":"global:m","limit":"100M"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if pn.cfg.Pool.PickByUIDForModel("a", "m") != nil {
		t.Fatal("new quota ignored previous usage")
	}
	if w := post("test-key", `{"model":"cn:m","limit":"100M"}`); w.Code != 400 {
		t.Fatal("cross realm accepted")
	}
	if w := post("test-key", `{"model":"m","limit":"0"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if pn.cfg.Pool.PickByUIDForModel("a", "m") == nil {
		t.Fatal("removal did not restore model")
	}
}
