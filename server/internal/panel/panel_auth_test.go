package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestPanelAuthToggle 面板免密开关（config panel.require_auth=false）。
//
// 开关只放行 /panel/*：关掉面板密码不该把 /v1/* 也变成无鉴权（那条链路的鉴权在
// internal/server 侧，独立于本开关）。缺省仍是「要密码」——老配置升级后行为不变。
func TestPanelAuthToggle(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})

	// ① 缺省（Live 里 PanelAuthDisabled=false）：无 key → 401
	on := New(Config{
		APIKey: "secret", Pool: p,
		Live: livecfg.New(livecfg.Snapshot{APIKey: "secret"}),
	})
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	rec := httptest.NewRecorder()
	on.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("默认应要求密钥：code=%d body=%s", rec.Code, rec.Body.String())
	}

	// ② 免密（PanelAuthDisabled=true）：无 key 也放行
	off := New(Config{
		APIKey: "secret", Pool: p,
		Live: livecfg.New(livecfg.Snapshot{APIKey: "secret", PanelAuthDisabled: true}),
	})
	req = httptest.NewRequest("GET", "/panel/api/overview", nil)
	rec = httptest.NewRecorder()
	off.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("免密开关应放行：code=%d body=%s", rec.Code, rec.Body.String())
	}

	// ③ 带正确 key 时同样放行（开关不影响正常鉴权路径）
	req = httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	on.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("正确密钥应放行：code=%d", rec.Code)
	}
}
