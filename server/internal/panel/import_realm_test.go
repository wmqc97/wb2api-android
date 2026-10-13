package panel

import (
	"encoding/json"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestImportRealmAutodetection(t *testing.T) {
	for _, tc := range []struct{ realm, domain, want string }{
		{"intl", "www.workbuddy.ai", "global"},
		{"vendor-special", "www.workbuddy.ai", "global"},
		{"cn", "www.workbuddy.ai", "global"},
		{" GLOBAL ", "https://WWW.WORKBUDDY.AI/auth", "global"},
		{"international", "", "global"}, {"海外版", "", "global"},
		{"intl", "www.codebuddy.cn", "cn"}, {"domestic", "", "cn"},
		{"", "copilot.tencent.com", "cn"}, {"", "", "cn"},
		{"unknown", "workbuddy.ai.evil.example", ""}, {"unknown", "notworkbuddy.ai", ""},
	} {
		raw, _ := json.Marshal(map[string]any{"uid": "test", "at": "synthetic-access", "rt": "synthetic-refresh", "realm": tc.realm, "domain": tc.domain})
		records, err := parseImportRecords(raw)
		if err != nil || len(records) != 1 {
			t.Fatal("cannot parse credential record")
		}
		if tc.want == "" {
			if records[0].Err == "" {
				t.Fatal("guessed unknown realm")
			}
			continue
		}
		if records[0].Err != "" {
			t.Fatalf("realm=%q domain=%q: %s", tc.realm, tc.domain, records[0].Err)
		}
		if got := records[0].Auth.RealmStored(); got != tc.want {
			t.Fatalf("realm=%q domain=%q got=%q want=%q", tc.realm, tc.domain, got, tc.want)
		}
	}
}
func TestImportMixedCNAndIntlFile(t *testing.T) {
	pn, _ := newImportPanel(t)
	pn.cfg.Upstream = nil
	payload := `[{"uid":"cn-one","at":"a","rt":"r","realm":"cn","domain":"www.codebuddy.cn"},{"uid":"global-one","at":"a","rt":"r","realm":"intl","domain":"www.workbuddy.ai"}]`
	code, r := postImport(t, pn, payload)
	if code != 200 || r["imported"] != float64(2) || r["skipped"] != float64(0) {
		t.Fatal(code, r)
	}
	if a := pn.cfg.Pool.AuthByUID("global-one"); a == nil || a.RealmStored() != "global" {
		t.Fatal("international account not preserved")
	}
}
func TestImportRealmAliasesAcrossFragments(t *testing.T) {
	records, err := parseImportRecords([]byte(`{"realm":"intl","auth":{"realm":"global","accessToken":"a","refreshToken":"r"},"account":{"uid":"one"}}`))
	if err != nil || len(records) != 1 || records[0].Err != "" {
		t.Fatal("equivalent realm aliases rejected")
	}
}
func TestImportIntlPreservesRealmWhenGlobalRoutingDisabled(t *testing.T) {
	auth.SetGlobalEnabled(false)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	pn, _ := newImportPanel(t)
	pn.cfg.Upstream = nil
	code, r := postImport(t, pn, `{"uid":"one","at":"a","rt":"r","realm":"intl","domain":"www.workbuddy.ai"}`)
	if code != 200 || r["imported"] != float64(1) {
		t.Fatal(code, r)
	}
	a := pn.cfg.Pool.AuthByUID("one")
	if a.RealmStored() != "global" {
		t.Fatal("CN routing override corrupted imported identity")
	}
}
