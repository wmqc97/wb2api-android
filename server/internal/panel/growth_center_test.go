package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func growthCenterTestPanel(t *testing.T, h http.Handler) *Panel {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn", AccessToken: "at", RefreshToken: "rt"})
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up,
		GrowthStateFile: filepath.Join(t.TempDir(), "data", "state.json")})
}

func authReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-key")
	return r
}

func TestGrowthCenterWiring(t *testing.T) {
	html, _ := os.ReadFile("index.html")
	for _, must := range []string{"data-view=\"growth\"", "id=\"view-growth\"", "growth_center.css", "growth_center.js"} {
		if !strings.Contains(string(html), must) {
			t.Errorf("index.html 缺 %s", must)
		}
	}
	js, _ := os.ReadFile("app.js")
	if !strings.Contains(string(js), "WBGrowthCenter?.enter()") {
		t.Error("app.js 缺成长页路由入口")
	}
	gcjs, _ := os.ReadFile("growth_center.js")
	if strings.Contains(string(gcjs), "onclick=") || strings.Contains(string(gcjs), "onerror=") {
		t.Error("growth_center.js 含内联事件处理器")
	}
	pn := New(Config{Version: "test", APIKey: "test-key"})
	for _, path := range []string{"/panel/growth_center.css", "/panel/growth_center.js", "/panel/api/growth/accounts"} {
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, authReq("GET", path, ""))
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s 未注册", path)
		}
	}
}

func TestGrowthCenterOverviewReadOnly(t *testing.T) {
	var calls atomic.Int64
	pn := growthCenterTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(404) }))
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, authReq("GET", "/panel/api/growth/accounts/u1", ""))
	if rec.Code != 200 {
		t.Fatalf("overview=%d %s", rec.Code, rec.Body.String())
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("页面读取打了 %d 次上游", n)
	}
}

func TestGrowthCenterRefreshAndSnapshot(t *testing.T) {
	var writes atomic.Int64
	pn := growthCenterTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.URL.Path != "/v2/billing/meter/checkin-activity-status" {
			writes.Add(1)
		}
		switch r.URL.Path {
		case "/activity/growth/streak":
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":9,"email":"leak@example.com"}}}`))
		case "/activity/growth/energy":
			w.Write([]byte(`{"code":0,"data":{"balance":37,"secret":"leak"}}`))
		case "/v2/billing/meter/checkin-activity-status":
			w.Write([]byte(`{"code":0,"data":{"active":true,"today_checked_in":false}}`))
		default:
			w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, authReq("POST", "/panel/api/growth/accounts/u1/refresh", ""))
	if rec.Code != 200 {
		t.Fatalf("refresh=%d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 300; i++ {
		if i > 0 && !pn.gc.job("u1").Running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pn.gc.job("u1").Running {
		t.Fatal("刷新任务未结束")
	}
	snap := pn.gc.growthSnap("u1")
	streak, _ := snap.Sections["streak"].Data["streak"].(map[string]any)
	if streak == nil || growthNum(streak["days"]) != 9 {
		t.Errorf("streak 快照=%v", snap.Sections["streak"].Data)
	}
	if _, leaked := streak["email"]; leaked {
		t.Error("快照泄露 email")
	}
	if growthNum(snap.Sections["energy"].Data["balance"]) != 37 {
		t.Error("energy 快照未保存")
	}
	if _, leaked := snap.Sections["energy"].Data["secret"]; leaked {
		t.Error("快照泄露 secret")
	}
	if n := writes.Load(); n != 0 {
		t.Errorf("快照刷新产生 %d 次写动作", n)
	}
}

func TestGrowthCenterManualActionAndIdempotency(t *testing.T) {
	var opens atomic.Int64
	pn := growthCenterTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/buddy/quota":
			w.Write([]byte(`{"code":0,"data":{"balance":50,"affordable":2,"cost_per_open":10,"max_open_count":5}}`))
		case "/activity/growth/buddy/open":
			opens.Add(1)
			w.Write([]byte(`{"code":0,"data":{"results":[{"template":{"name":"小绿","rarity":"R"}}]}}`))
		default:
			w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	first := httptest.NewRecorder()
	pn.ServeHTTP(first, authReq("POST", "/panel/api/growth/accounts/u1/action", `{"action":"open"}`))
	if first.Code != 200 {
		t.Fatalf("首次开盒=%d %s", first.Code, first.Body.String())
	}
	var out growthResult
	_ = json.Unmarshal(first.Body.Bytes(), &out)
	if !out.OK || !strings.Contains(out.Reason, "小绿") {
		t.Errorf("开盒回执=%+v", out)
	}
	second := httptest.NewRecorder()
	pn.ServeHTTP(second, authReq("POST", "/panel/api/growth/accounts/u1/action", `{"action":"open"}`))
	if second.Code == 200 {
		t.Error("同日重复开盒应拒绝")
	}
	if n := opens.Load(); n != 1 {
		t.Errorf("开盒调用=%d want 1", n)
	}
	if owner, ok := pn.gc.acquireLease(); !ok {
		t.Error("租约未释放")
	} else {
		pn.gc.releaseLease(owner)
	}
}

func TestGrowthCenterEnterpriseGate(t *testing.T) {
	pn := growthCenterTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("企业号不应打上游 %s", r.URL.Path)
		w.WriteHeader(404)
	}))
	pn.cfg.Pool.Add(&auth.Auth{UID: "ent", EnterpriseID: "corp", Domain: "www.codebuddy.cn"})
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, authReq("GET", "/panel/api/growth/accounts/ent", ""))
	if rec.Code == 200 {
		t.Error("企业号不应返回成长快照")
	}
	for _, a := range pn.growthAccounts() {
		if a["uid"] == "ent" {
			t.Error("选择器不应列出企业号")
		}
	}
}

func TestGrowthCenterPlanRoundCursorAndLease(t *testing.T) {
	var reports atomic.Int64
	day := growthDay(time.Now())
	pn := growthCenterTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/report":
			reports.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		case r.URL.Path == "/activity/growth/heatmap":
			w.Write([]byte(`{"code":0,"data":{"today":{"date":"` + day + `","is_active":true,"score":1}}}`))
		case r.URL.Path == "/v2/billing/meter/checkin-activity-status":
			w.Write([]byte(`{"code":0,"data":{"active":true,"today_checked_in":true}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			w.Write([]byte(`{"code":0,"data":{"state":"traveling"}}`))
		case r.URL.Path == "/activity/growth/buddy/info":
			w.Write([]byte(`{"code":0,"data":{"buddy":{"name":"小绿"}}}`))
		case r.URL.Path == "/activity/growth/lottery/summary":
			w.Write([]byte(`{"code":0,"data":{"chances":0,"module":{"enabled":false}}}`))
		default:
			w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	pn.gc.runPlanRound()
	if n := reports.Load(); n != 0 {
		t.Errorf("已点亮仍发 %d 次活跃上报", n)
	}
	rows, _, _ := pn.gc.records("u1", 0, 20)
	if len(rows) != 1 || rows[0].Source != "schedule" {
		t.Errorf("巡检记录=%+v", rows)
	}
	pn.gc.mu.Lock()
	cursor, lease := pn.gc.store.Cursor.ID, pn.gc.store.Lease.Owner
	pn.gc.mu.Unlock()
	if cursor != "u1" {
		t.Errorf("cursor=%q", cursor)
	}
	if lease != "" {
		t.Errorf("租约未释放 %q", lease)
	}
}

func TestGrowthCenterRunWaitsForManualLease(t *testing.T) {
	pn := growthCenterTestPanel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("租约占用期间不应发巡检请求：%s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	owner, ok := pn.gc.acquireLease()
	if !ok {
		t.Fatal("测试前置：租约应可取")
	}
	if pn.gc.startPlanRound() {
		t.Error("手动动作占租约时立即巡检应返回 false")
	}
	if pn.gc.planRunning() {
		t.Error("不应把巡检状态置为 running")
	}
	pn.gc.releaseLease(owner)
}
