package panel

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestCredentialExportAuthAndRoundTrip(t *testing.T) {
	pn, _ := newImportPanel(t)
	pn.cfg.Upstream = nil
	a := &auth.Auth{UID: "export-one", AccessToken: "synthetic-at", RefreshToken: "synthetic-rt", ExpiresAt: 1893456000, Nickname: "Example", Domain: "www.workbuddy.ai", EnterpriseID: "ent-example", DeviceToken: "synthetic-device", FilePath: "must-not-be-exported"}
	auth.BackfillRealmFor(a, "global")
	pn.cfg.Pool.Add(a)
	get := func(path, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		pn.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/panel/api/accounts/export-one/export", "/panel/api/accounts/export"} {
		for _, key := range []string{"", "wrong"} {
			w := get(path, key)
			if w.Code != 401 || strings.Contains(w.Body.String(), "synthetic") {
				t.Fatal("export auth failure")
			}
		}
		w := get(path, "test-key")
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment;") {
			t.Fatal("export response contract failed", w.Code)
		}
		if strings.Contains(w.Body.String(), "must-not-be-exported") {
			t.Fatal("internal path leaked")
		}
		records, err := parseImportRecords(w.Body.Bytes())
		if err != nil || len(records) != 1 || records[0].Err != "" {
			t.Fatal("export cannot be reimported")
		}
		b := records[0].Auth
		if b.AccessToken != a.AccessToken || b.RefreshToken != a.RefreshToken || b.EnterpriseID != a.EnterpriseID || b.DeviceToken != a.DeviceToken || b.RealmStored() != "global" {
			t.Fatal("export lost credential fields")
		}
		other, _ := newImportPanel(t)
		other.cfg.Upstream = nil
		code, result := postImport(t, other, w.Body.String())
		if code != 200 || result["imported"] != float64(1) {
			t.Fatal("export-import request failed")
		}
	}
	if w := get("/panel/api/accounts/missing/export", "test-key"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	w := get("/panel/api/overview", "test-key")
	if strings.Contains(w.Body.String(), a.AccessToken) || strings.Contains(w.Body.String(), a.RefreshToken) {
		t.Fatal("ordinary overview exposed credentials")
	}
	if a.FilePath != "must-not-be-exported" {
		t.Fatal("export mutated source file path")
	}
}
func TestCredentialExportBulkAndEmpty(t *testing.T) {
	pn, _ := newImportPanel(t)
	call := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/panel/api/accounts/export", nil)
		r.Header.Set("Authorization", "Bearer test-key")
		pn.ServeHTTP(w, r)
		return w
	}
	if w := call(); w.Code != 400 {
		t.Fatal("empty export should explain no accounts")
	}
	for _, uid := range []string{"cn", "global"} {
		pn.cfg.Pool.Add(&auth.Auth{UID: uid, AccessToken: "synthetic", RefreshToken: "synthetic"})
	}
	w := call()
	var rows []json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows) != 2 {
		t.Fatal("bulk export lost accounts")
	}
}
