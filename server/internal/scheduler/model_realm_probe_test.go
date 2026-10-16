package scheduler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestCooldownProbeSkipsWrongRealmUntilCatalogAddsModel(t *testing.T) {
	previous := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(previous) })
	stub := &probeStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			io.WriteString(w, `{"code":0,"data":{"models":[{"id":"cn-only"}]}}`)
			return
		}
		if r.URL.Path == "/v3/config" {
			http.NotFound(w, r)
			return
		}
		stub.calls.Add(1)
		stub.sseOK(w)
	}))
	defer srv.Close()
	s, p := probeScheduler(t, srv)
	if _, err := s.cfg.Upstream.FetchModels(p.AuthByUID("u1")); err != nil {
		t.Fatal(err)
	}
	p.Add(&auth.Auth{UID: "global", Domain: "www.workbuddy.ai", AccessToken: "global-at", ExpiresAt: 9999999999})
	expireModelCooldown(t, p, "global", "cn-only")
	res := s.RunCooldownProbeNow()
	if res.Probed != 0 || res.SkippedRealm != 1 || stub.calls.Load() != 0 {
		t.Fatalf("wrong-realm cooldown must not trigger chat: result=%+v calls=%d", res, stub.calls.Load())
	}
	// A later successful global directory refresh makes the model shared.
	s.cfg.Upstream.GlobalEnabled = true
	s.cfg.Upstream.ChatBaseGlobal = srv.URL
	if _, err := s.cfg.Upstream.FetchModels(p.AuthByUID("global")); err != nil {
		t.Fatal(err)
	}
	res = s.RunCooldownProbeNow()
	if res.Probed != 1 || stub.calls.Load() != 1 {
		t.Fatalf("new global support should permit probing: result=%+v calls=%d", res, stub.calls.Load())
	}
}
