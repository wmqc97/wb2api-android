package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestModelCatalogKeepsDirectoryWhenOverlayStalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/config" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done() // Also covers a hung response body after headers.
			return
		}
		io.WriteString(w, `{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash","credits":"x0.00","maxOutputTokens":8192}]}}`)
	}))
	defer srv.Close()
	c := New()
	c.ChatBaseCN = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	models, _, err := c.FetchModelsDiagContext(ctx, &auth.Auth{UID: "overlay", Domain: "www.codebuddy.cn", AccessToken: "at"})
	if err != nil || len(models) != 1 || models[0].ID != "deepseek-v4.1-flash" {
		t.Fatalf("optional overlay discarded directory: models=%v err=%v", models, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("overlay cancellation did not reach HTTP response body")
	}
}

func TestModelCatalogFallsBackToV3(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/config" {
			http.Error(w, "unavailable", 503)
			return
		}
		io.WriteString(w, `{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash","credits":"x0.00","maxOutputTokens":8192,"reasoning":{"defaultEffort":"high","supportedEfforts":["high"]}}]}}`)
	}))
	defer srv.Close()
	c := New()
	c.ChatBaseCN = srv.URL
	models, diag, err := c.FetchModelsDiag(&auth.Auth{UID: "fallback", Domain: "www.codebuddy.cn", AccessToken: "at"})
	if err != nil || len(models) != 1 || diag.Path != "/v3/config" || models[0].Credits != "x0.00" {
		t.Fatalf("v3 fallback: models=%v path=%s err=%v", models, diag.Path, err)
	}
	if c.defaultEffortsSnapshot("cn")["deepseek-v4.1-flash"] != "high" {
		t.Fatal("v3 fallback did not update reasoning capability cache")
	}
}
