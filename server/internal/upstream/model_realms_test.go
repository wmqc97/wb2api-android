package upstream

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestModelRealmCatalogSurvivesFetchFailureAndUpdatesOnSuccess(t *testing.T) {
	c := New()
	c.recordModelCatalog("cn", []ModelInfo{{ID: "kimi-k3.1"}, {ID: "shared"}})
	c.recordModelCatalog("global", []ModelInfo{{ID: "global-only"}, {ID: "shared"}})
	for model, want := range map[string]string{"kimi-k3.1": "cn", "global-only": "global", "shared": "", "unknown": ""} {
		if got := c.ExclusiveModelRealm(model); got != want {
			t.Fatalf("owner(%s)=%s want %s", model, got, want)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "temporary failure", 503) }))
	defer srv.Close()
	c.ChatBaseCN = srv.URL
	if _, err := c.FetchModels(&auth.Auth{UID: "cn", Domain: "www.codebuddy.cn", AccessToken: "at"}); err == nil {
		t.Fatal("expected catalog fetch failure")
	}
	if c.ExclusiveModelRealm("kimi-k3.1") != "cn" {
		t.Fatal("failed refresh erased model grouping")
	}
	c.recordModelCatalog("global", []ModelInfo{{ID: "kimi-k3.1"}})
	if cn, gl := c.ModelRealms("kimi-k3.1"); !cn || !gl {
		t.Fatal("new global listing did not enable shared routing")
	}
	c.recordModelCatalog("cn", []ModelInfo{{ID: "shared"}})
	if c.ExclusiveModelRealm("kimi-k3.1") != "global" {
		t.Fatal("successful catalog replacement did not update ownership")
	}
}

func TestModelRealmCatalogConcurrentRefresh(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	for _, realm := range []string{"cn", "global"} {
		wg.Add(1)
		go func(realm string) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				c.recordModelCatalog(realm, []ModelInfo{{ID: "shared"}})
				c.ModelRealms("shared")
			}
		}(realm)
	}
	wg.Wait()
	if cn, gl := c.ModelRealms("shared"); !cn || !gl {
		t.Fatal("lost realm during concurrent refresh")
	}
}
