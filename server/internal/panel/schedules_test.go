package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func scheduleTestPanel(t *testing.T) *Panel {
	t.Helper()
	dir := t.TempDir()
	return New(Config{Version: "test", APIKey: "test-key", GrowthStateFile: dir + "/data/state.json"})
}

func scheduleAuthReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-key")
	return r
}

func TestSchedulesWiring(t *testing.T) {
	html, _ := os.ReadFile("index.html")
	for _, must := range []string{"data-view=\"schedules\"", "id=\"view-schedules\"", "schedules.js", "id=\"scheduleModal\""} {
		if !strings.Contains(string(html), must) {
			t.Errorf("index.html 缺 %s", must)
		}
	}
	js, _ := os.ReadFile("app.js")
	if !strings.Contains(string(js), "WBSchedules?.enter()") {
		t.Error("app.js 缺定时任务路由入口")
	}
	p := New(Config{Version: "test", APIKey: "test-key"})
	for _, path := range []string{"/panel/schedules.js", "/panel/api/schedules", "/panel/api/schedules/status"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, scheduleAuthReq("GET", path, ""))
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s 未注册", path)
		}
	}
}

func TestSchedulesSeedAndCRUD(t *testing.T) {
	pn := New(Config{Version: "test", APIKey: "test-key"})
	if pn.sm != nil {
		t.Fatal("无 Pool/Upstream 的单测面板不应创建状态文件")
	}
	// 直接构造管理器（不依赖池/上游），只测 CRUD 与持久化。
	m := newScheduleManager(pn, t.TempDir()+"/data/state.json")
	pn.sm = m
	if len(m.list()) < 9 {
		t.Errorf("默认任务数=%d want >=9", len(m.list()))
	}
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, scheduleAuthReq("GET", "/panel/api/schedules", ""))
	var payload struct {
		Items []map[string]any `json:"items"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &payload) != nil || len(payload.Items) == 0 {
		t.Fatalf("列表=%s", rec.Body.String())
	}
	// 新建：未知任务拒绝；合法任务返回 id。
	bad := httptest.NewRecorder()
	pn.ServeHTTP(bad, scheduleAuthReq("POST", "/panel/api/schedules", `{"task":"nope"}`))
	if bad.Code == http.StatusOK {
		t.Error("未知任务应 400")
	}
	create := httptest.NewRecorder()
	pn.ServeHTTP(create, scheduleAuthReq("POST", "/panel/api/schedules", `{"task":"daily_checkin","name":"我的签到","interval_minutes":30,"timing":{"mode":"interval","time":"09:00","weekdays":[0],"monthday":1}}`))
	if create.Code != http.StatusOK {
		t.Fatalf("新建=%d %s", create.Code, create.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(create.Body.Bytes(), &created)
	if created.ID == 0 {
		t.Fatal("新建未返回 id")
	}
	r, ok := m.row(created.ID)
	if !ok || r.Name != "我的签到" || r.IntervalMinutes != 30 {
		t.Errorf("新建行=%+v ok=%v", r, ok)
	}
	// 编辑：改名 + 周期。
	patch := httptest.NewRecorder()
	pn.ServeHTTP(patch, scheduleAuthReq("PATCH", "/panel/api/schedules/"+strconv.FormatInt(created.ID, 10), `{"name":"改名","interval_minutes":45,"timing":{"mode":"daily","time":"08:30","weekdays":[1],"monthday":1}}`))
	if patch.Code != http.StatusOK {
		t.Fatalf("编辑=%d %s", patch.Code, patch.Body.String())
	}
	r, _ = m.row(created.ID)
	if r.Name != "改名" || r.IntervalMinutes != 45 || r.Timing.Mode != "daily" || r.Timing.Time != "08:30" {
		t.Errorf("编辑后=%+v", r)
	}
	// 启停。
	tog := httptest.NewRecorder()
	pn.ServeHTTP(tog, scheduleAuthReq("POST", "/panel/api/schedules/"+strconv.FormatInt(created.ID, 10)+"/toggle", ""))
	if tog.Code != http.StatusOK {
		t.Fatalf("启停=%d", tog.Code)
	}
	r, _ = m.row(created.ID)
	if r.Enabled {
		t.Error("toggle 后应禁用")
	}
	// 删除。
	del := httptest.NewRecorder()
	pn.ServeHTTP(del, scheduleAuthReq("DELETE", "/panel/api/schedules/"+strconv.FormatInt(created.ID, 10), ""))
	if del.Code != http.StatusOK {
		t.Fatalf("删除=%d", del.Code)
	}
	if _, ok := m.row(created.ID); ok {
		t.Error("删除后仍存在")
	}
}

func TestScheduleTimingLabelsAndNextRun(t *testing.T) {
	base := ScheduleRow{IntervalMinutes: 60, Task: "daily_checkin", Timing: defaultTiming()}
	if got := cycleLabel(base); got != "每 1 小时" {
		t.Errorf("preset label=%q", got)
	}
	base.Timing = Timing{Mode: "daily", Time: "09:30", Weekdays: []int{0}, Monthday: 1}
	if got := cycleLabel(base); got != "每天 09:30" {
		t.Errorf("daily label=%q", got)
	}
	now := time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC) // 北京时间 09:00
	next := nextRun(base, now)
	if got := next.In(scheduleCST).Format("2006-01-02 15:04"); got != "2026-11-14 09:30" {
		t.Errorf("daily next=%s want 2026-11-14 09:30", got)
	}
	base.Timing = Timing{Mode: "weekly", Time: "10:15", Weekdays: []int{5}, Monthday: 1} // 周六
	next = nextRun(base, now)
	if got := next.In(scheduleCST).Format("2006-01-02 15:04"); got != "2026-11-14 10:15" {
		t.Errorf("weekly next=%s want 2026-11-14 10:15", got)
	}
	base.Timing = Timing{Mode: "monthly", Time: "08:00", Weekdays: []int{0}, Monthday: 31}
	next = nextRun(base, now)
	if got := next.In(scheduleCST).Format("2006-01-02 15:04"); got != "2026-11-30 08:00" {
		t.Errorf("monthly next=%s want 2026-11-30 08:00（当月无 31 日）", got)
	}
}

func TestSchedulesRunRecordsResult(t *testing.T) {
	pn := New(Config{Version: "test", APIKey: "test-key"})
	m := newScheduleManager(pn, t.TempDir()+"/data/state.json")
	pn.sm = m
	// 未装配 Scheduler 的任务会返回结构化错误，但仍必须回填 last_result。
	var id int64
	for _, item := range m.list() {
		if item["task"] == "daily_checkin" {
			id = item["id"].(int64)
			break
		}
	}
	if id == 0 {
		t.Fatal("缺少 daily_checkin 默认任务")
	}
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, scheduleAuthReq("POST", "/panel/api/schedules/"+strconv.FormatInt(id, 10)+"/run", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("run=%d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 200; i++ {
		r, _ := m.row(id)
		if r.LastResult != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	r, _ := m.row(id)
	var result map[string]any
	if json.Unmarshal([]byte(r.LastResult), &result) != nil {
		t.Fatalf("last_result 不是完整 JSON: %q", r.LastResult)
	}
	if result["error"] == nil {
		t.Errorf("未装配调度器时应记录结构化错误：%v", result)
	}
	if r.LastRunAt == "" || r.NextRunAt == "" {
		t.Error("运行后应回填 last_run_at / next_run_at")
	}
}

func TestSchedulesGrowthPlanView(t *testing.T) {
	pn := New(Config{Version: "test", APIKey: "test-key"})
	m := newScheduleManager(pn, t.TempDir()+"/data/state.json")
	pn.sm = m
	view := m.growthPlanView()
	if view == nil || view["enabled"] != true || view["cycle_label"] != "每 1 小时" {
		t.Errorf("成长计划排程视图=%v", view)
	}
}
