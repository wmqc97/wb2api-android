package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestImportOwnSavedAuthRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := &auth.Auth{UID: "own-account", AccessToken: "access-example", RefreshToken: "refresh-example", ExpiresAt: 1893456000, Domain: "www.workbuddy.ai", EnterpriseID: "ent-example", Nickname: "Round trip", DeviceToken: "device-example", FilePath: filepath.Join(dir, "original.json")}
	auth.BackfillRealmFor(a, "global")
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	pn, out := newImportPanel(t)
	pn.cfg.Upstream = nil
	code, result := postImport(t, pn, string(raw))
	if code != 200 || result["imported"] != float64(1) {
		t.Fatalf("import failed: code=%d result=%v", code, result)
	}
	saved, err := auth.LoadFile(filepath.Join(out, "workbuddy-own-account.json"))
	if err != nil {
		t.Fatal(err)
	}
	if saved.UID != a.UID || saved.AccessToken != a.AccessToken || saved.RefreshToken != a.RefreshToken || saved.ExpiresAt != a.ExpiresAt || saved.EnterpriseID != a.EnterpriseID || saved.DeviceToken != a.DeviceToken || saved.Realm() != "global" {
		t.Fatal("round-trip lost auth fields")
	}
}

func TestImportFormatsAndExpiry(t *testing.T) {
	const flat = `{"user_id":"one","access_token":"a","refresh_token":"r","expires_at":"1893456000000","realm":"global"}`
	for name, raw := range map[string]string{
		"flat": flat, "array": "[" + flat + "]", "wrapped": `{"data":{"accounts":[` + flat + `]}}`, "dict": `{"export":{"one":` + flat + `}}`,
		"bom":         "\xef\xbb\xbf" + flat,
		"sibling":     `{"auth":{"accessToken":"a","refreshToken":"r","expiresAt":1893456000,"realm":"global"},"account":{"uid":"one"}}`,
		"capitalized": `{"UID":"one","AccessToken":"a","RefreshToken":"r","ExpiresAt":"2030-01-01T00:00:00Z","Realm":"global"}`,
		"encoded":     `{"storage":` + strconvJSON(flat) + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			rs, err := parseImportRecords([]byte(raw))
			if err != nil || len(rs) != 1 || rs[0].Err != "" {
				t.Fatalf("parse failed: %v %+v", err, rs)
			}
			a := rs[0].Auth
			if a.UID != "one" || a.ExpiresAt != 1893456000 || a.Realm() != "global" {
				t.Fatal("identity/expiry/realm mismatch")
			}
		})
	}
}
func strconvJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestImportNeverCombinesAccountsOrEchoesTokens(t *testing.T) {
	raw := `{"accounts":[{"uid":"one","accessToken":"secret-first"},{"uid":"two","refreshToken":"secret-second"}],"metadata":{"uid":"wrong"}}`
	rs, err := parseImportRecords([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Auth != nil || r.Err == "" {
			t.Fatal("combined incomplete credentials")
		}
		if strings.Contains(r.Err, "secret") {
			t.Fatal("credential echoed")
		}
	}
	for _, raw := range []string{`{"uid":"one","accessToken":"secret-a","access_token":"secret-b","refreshToken":"r"}`, `{"uid":"../outside","accessToken":"a","refreshToken":"r"}`} {
		rs, err := parseImportRecords([]byte(raw))
		if err != nil || len(rs) != 1 || rs[0].Err == "" {
			t.Fatal("ambiguous or unsafe account accepted")
		}
	}
}
func TestImportDuplicatesAndMissingExpiry(t *testing.T) {
	pn, _ := newImportPanel(t)
	pn.cfg.Upstream = nil
	entry := `{"uid":"one","accessToken":"a","refreshToken":"r"}`
	code, r := postImport(t, pn, "["+entry+","+entry+"]")
	if code != 200 || r["imported"] != float64(1) || r["skipped"] != float64(1) {
		t.Fatal(code, r)
	}
	if a := pn.cfg.Pool.AuthByUID("one"); !a.NeedsRefresh(0) {
		t.Fatal("unknown expiry should refresh, not assume one year")
	}
}
