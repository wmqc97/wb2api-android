// schedules.go Go 版「定时任务」管理面板（参考项目 admin/routers/schedules.py
// + admin/schedule_timing.py + admin/console/index.html 的定时任务段）。
//
// 与 Python 参考的对应：
//   - SQLAlchemy Schedule 表 → App 私有目录 data/schedules.json（原子落盘）；
//   - _loop 的 15 秒轮询 → 15 秒 ticker goroutine，支持 ctx 优雅退出；
//   - timing_of / next_run_at / cycle_label → Timing / nextRun / cycleLabel；
//   - run_task → 复用本项目既有 Scheduler / Panel 任务入口；
//   - /api/schedules CRUD → /panel/api/schedules/*，沿用面板 Bearer 鉴权。
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var scheduleCST = time.FixedZone("CST", 8*3600)

const scheduleStoreName = "schedules.json"

var scheduleTaskLabels = map[string]string{
	"refresh_balances":     "刷新平台总积分",
	"refresh_credits":      "更新积分到期快照",
	"sync_models":          "同步模型列表",
	"daily_checkin":        "签到领取积分",
	"refresh_growth_tasks": "更新成长任务列表",
	"run_growth_tasks":     "自动做成长任务",
	"run_growth_plan":      "WorkBuddy 成长计划巡检",
	"keepalive_tokens":     "token 保活刷新",
	"ban_recheck":          "风控封号复核与自动解封",
	"travel":               "猫猫旅行巡检",
	"activity":             "每日活跃上报",
	"school":               "开学季活动",
	"cooldown_probe":       "冷却探活",
	"streak_bonus":         "连登奖励与抽奖",
	"blackcat":             "夜猫子任务",
}

var scheduleDefaultNames = map[string][2]string{
	"refresh_balances":     {"整点刷新平台总积分", "刷新平台总积分"},
	"refresh_credits":      {"每 2 小时更新积分到期快照", "更新积分到期快照"},
	"sync_models":          {"每日同步模型列表", "同步模型列表"},
	"daily_checkin":        {"每日签到领取积分", "签到领取积分"},
	"refresh_growth_tasks": {"每日更新成长任务列表", "更新成长任务列表"},
	"run_growth_tasks":     {"每日自动做成长任务", "自动做成长任务"},
	"run_growth_plan":      {"WorkBuddy 成长计划巡检", "WorkBuddy 成长计划巡检"},
	"keepalive_tokens":     {"每日 token 保活刷新", "token 保活刷新"},
}

type Timing struct {
	Mode     string `json:"mode"`
	Time     string `json:"time"`
	Weekdays []int  `json:"weekdays"`
	Monthday int    `json:"monthday"`
}

func defaultTiming() Timing {
	return Timing{Mode: "preset", Time: "09:00", Weekdays: []int{0}, Monthday: 1}
}

func normalizeTiming(t Timing) Timing {
	out := defaultTiming()
	if t.Mode != "" {
		out.Mode = t.Mode
	}
	switch out.Mode {
	case "preset", "interval", "daily", "weekly", "monthly":
	default:
		out.Mode = "preset"
	}
	if len(t.Time) == 5 && t.Time[2] == byte(58) {
		if h, err := strconv.Atoi(t.Time[:2]); err == nil && h >= 0 && h < 24 {
			if m, err := strconv.Atoi(t.Time[3:]); err == nil && m >= 0 && m < 60 {
				out.Time = t.Time
			}
		}
	}
	weekdays := []int{}
	for _, d := range t.Weekdays {
		if d >= 0 && d <= 6 {
			weekdays = append(weekdays, d)
		}
	}
	sort.Ints(weekdays)
	if len(weekdays) > 0 {
		out.Weekdays = weekdays
	}
	if t.Monthday >= 1 && t.Monthday <= 31 {
		out.Monthday = t.Monthday
	}
	return out
}

type ScheduleRow struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Task            string `json:"task"`
	IntervalMinutes int    `json:"interval_minutes"`
	Timing          Timing `json:"timing"`
	Enabled         bool   `json:"enabled"`
	StopAfter       string `json:"stop_after"`
	NextRunAt       string `json:"next_run_at"`
	LastRunAt       string `json:"last_run_at"`
	LastResult      string `json:"last_result"`
}

type scheduleStore struct {
	Rows   []ScheduleRow `json:"rows"`
	NextID int64         `json:"next_id"`
}

type ScheduleManager struct {
	mu      sync.Mutex
	path    string
	store   scheduleStore
	panel   *Panel
	running map[int64]bool
	wake    chan struct{}
	once    sync.Once
}

func newScheduleManager(p *Panel, stateFile string) *ScheduleManager {
	name := scheduleStoreName
	if dir := filepath.Dir(stateFile); dir != "" && dir != "." {
		name = filepath.Join(dir, name)
	}
	m := &ScheduleManager{path: name, store: scheduleStore{Rows: []ScheduleRow{}, NextID: 1},
		panel: p, running: map[int64]bool{}, wake: make(chan struct{}, 1)}
	m.load()
	m.ensureDefaults()
	return m
}

func (m *ScheduleManager) load() {
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var s scheduleStore
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	if s.Rows == nil {
		s.Rows = []ScheduleRow{}
	}
	if s.NextID < 1 {
		s.NextID = 1
	}
	for i := range s.Rows {
		s.Rows[i].Timing = normalizeTiming(s.Rows[i].Timing)
		if s.Rows[i].IntervalMinutes < 1 {
			s.Rows[i].IntervalMinutes = 60
		}
	}
	m.store = s
}

func (m *ScheduleManager) saveLocked() {
	raw, err := json.Marshal(m.store)
	if err != nil {
		return
	}
	if dir := filepath.Dir(m.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	tmp := m.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, m.path)
}

func (m *ScheduleManager) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func scheduleStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
func parseStamp(v string) time.Time {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func (m *ScheduleManager) ensureDefaults() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	has := map[string]bool{}
	for _, r := range m.store.Rows {
		has[r.Task] = true
	}
	add := func(task, name string, minutes int, delay time.Duration) {
		if has[task] {
			return
		}
		r := ScheduleRow{ID: m.store.NextID, Name: name, Task: task, IntervalMinutes: minutes,
			Timing: defaultTiming(), Enabled: true, NextRunAt: scheduleStamp(now.Add(delay))}
		m.store.NextID++
		m.store.Rows = append(m.store.Rows, r)
		has[task] = true
	}
	// 本项目已有内建 Scheduler 的整点签到/旅行/保活排程，首次播种刻意错峰：
	// 全部 next_run_at=now 会让 App 一启动就对上游打一整轮重复请求。
	if len(m.store.Rows) == 0 {
		add("refresh_balances", "整点刷新平台总积分", 60, time.Minute)
		add("refresh_credits", "每 2 小时更新积分到期快照", 120, 3*time.Minute)
		add("sync_models", "每日同步模型列表", 1440, 5*time.Minute)
		add("daily_checkin", "每日签到领取积分", 1440, 7*time.Minute)
		add("refresh_growth_tasks", "每日更新成长任务列表", 1440, 9*time.Minute)
		add("run_growth_tasks", "每日自动做成长任务", 1440, 12*time.Minute)
		add("run_growth_plan", "WorkBuddy 成长计划巡检", 60, 5*time.Minute)
		add("keepalive_tokens", "每日 token 保活刷新", 1440, 15*time.Minute)
		add("ban_recheck", "风控封号复核与自动解封", 30, 2*time.Minute)
	} else {
		add("refresh_credits", "每 2 小时更新积分到期快照", 120, 3*time.Minute)
		add("ban_recheck", "风控封号复核与自动解封", 30, 2*time.Minute)
		add("run_growth_plan", "WorkBuddy 成长计划巡检", 60, 5*time.Minute)
	}
	m.saveLocked()
}

func cycleLabel(r ScheduleRow) string {
	t := normalizeTiming(r.Timing)
	if t.Mode == "preset" && r.Task == "keepalive_tokens" {
		return "每天 22:00（系统时区）"
	}
	switch t.Mode {
	case "daily":
		return "每天 " + t.Time
	case "weekly":
		names := []rune("一二三四五六日")
		parts := []string{}
		for _, d := range t.Weekdays {
			if d >= 0 && d < len(names) {
				parts = append(parts, "周"+string(names[d]))
			}
		}
		return "每" + strings.Join(parts, "、") + " " + t.Time
	case "monthly":
		return fmt.Sprintf("每月 %d 日 %s", t.Monthday, t.Time)
	default:
		minutes := r.IntervalMinutes
		if minutes < 1 {
			minutes = 60
		}
		for _, u := range []struct {
			size  int
			label string
		}{{10080, "周"}, {1440, "天"}, {60, "小时"}} {
			if minutes%u.size == 0 {
				return fmt.Sprintf("每 %d %s", minutes/u.size, u.label)
			}
		}
		return fmt.Sprintf("每 %d 分钟", minutes)
	}
}

func displayName(r ScheduleRow) string {
	base := scheduleTaskLabels[r.Task]
	if base == "" {
		base = r.Task
	}
	if pair, ok := scheduleDefaultNames[r.Task]; ok {
		if r.Name == "" || r.Name == pair[0] || r.Name == pair[1] || r.Name == "token 保活（按整点）" {
			return cycleLabel(r) + " · " + pair[1]
		}
	}
	return r.Name
}

func nextRun(r ScheduleRow, now time.Time) time.Time {
	t := normalizeTiming(r.Timing)
	minutes := r.IntervalMinutes
	if minutes < 1 {
		minutes = 60
	}
	if t.Mode == "preset" && r.Task == "keepalive_tokens" {
		return nextDailyAt(now, 22, 0)
	}
	if t.Mode == "preset" || t.Mode == "interval" {
		return now.UTC().Add(time.Duration(minutes) * time.Minute)
	}
	local := now.In(scheduleCST)
	hh, mm := 9, 0
	if v, err := strconv.Atoi(t.Time[:2]); err == nil {
		hh = v
	}
	if v, err := strconv.Atoi(t.Time[3:]); err == nil {
		mm = v
	}
	if t.Mode == "monthly" {
		year, month := local.Year(), local.Month()
		for i := 0; i < 3; i++ {
			day := t.Monthday
			if max := time.Date(year, month+1, 0, 0, 0, 0, 0, scheduleCST).Day(); day > max {
				day = max
			}
			candidate := time.Date(year, month, day, hh, mm, 0, 0, scheduleCST)
			if candidate.After(local) {
				return candidate.UTC()
			}
			if month == time.December {
				year, month = year+1, time.January
			} else {
				month++
			}
		}
	}
	for days := 0; days < 8; days++ {
		day := local.AddDate(0, 0, days)
		candidate := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, scheduleCST)
		if !candidate.After(local) {
			continue
		}
		if t.Mode == "daily" {
			return candidate.UTC()
		}
		wd := int(candidate.Weekday())
		if wd == 0 {
			wd = 6
		} else {
			wd--
		}
		for _, d := range t.Weekdays {
			if d == wd {
				return candidate.UTC()
			}
		}
	}
	return now.UTC().Add(time.Duration(minutes) * time.Minute)
}

func nextDailyAt(now time.Time, hour, minute int) time.Time {
	local := now.In(scheduleCST)
	candidate := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, scheduleCST)
	if !candidate.After(local) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate.UTC()
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (m *ScheduleManager) view(r ScheduleRow, now time.Time) map[string]any {
	next := parseStamp(r.NextRunAt)
	return map[string]any{
		"id": r.ID, "name": r.Name, "display_name": displayName(r),
		"timing": r.Timing, "cycle_label": cycleLabel(r),
		"task": r.Task, "task_label": scheduleTaskLabels[r.Task],
		"interval_minutes": r.IntervalMinutes, "enabled": r.Enabled,
		"last_run_at": nilIfEmpty(r.LastRunAt), "next_run_at": nilIfEmpty(r.NextRunAt),
		"last_result": r.LastResult, "stop_after": nilIfEmpty(r.StopAfter),
		"due_soon": r.Enabled && (next.IsZero() || !next.After(now)),
	}
}

func (m *ScheduleManager) list() []map[string]any {
	m.mu.Lock()
	rows := append([]ScheduleRow{}, m.store.Rows...)
	m.mu.Unlock()
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	now := time.Now().UTC()
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, m.view(r, now))
	}
	return out
}

func (m *ScheduleManager) row(id int64) (ScheduleRow, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.store.Rows {
		if r.ID == id {
			return r, true
		}
	}
	return ScheduleRow{}, false
}

type scheduleInput struct {
	Name            string  `json:"name"`
	Task            string  `json:"task"`
	IntervalMinutes int     `json:"interval_minutes"`
	Timing          *Timing `json:"timing"`
	Enabled         *bool   `json:"enabled"`
	StopAfter       *string `json:"stop_after"`
}

func (m *ScheduleManager) saveRow(r ScheduleRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.store.Rows {
		if m.store.Rows[i].ID == r.ID {
			m.store.Rows[i] = r
			m.saveLocked()
			return
		}
	}
}

func (m *ScheduleManager) create(in scheduleInput) (int64, error) {
	if _, ok := scheduleTaskLabels[in.Task]; !ok {
		return 0, fmt.Errorf("未知任务类型")
	}
	if in.IntervalMinutes < 1 {
		in.IntervalMinutes = 60
	}
	if in.IntervalMinutes > 5256000 {
		return 0, fmt.Errorf("执行间隔超出范围")
	}
	t := defaultTiming()
	if in.Timing != nil {
		t = normalizeTiming(*in.Timing)
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	stop := ""
	if in.StopAfter != nil {
		stop = strings.TrimSpace(*in.StopAfter)
	}
	m.mu.Lock()
	id := m.store.NextID
	m.store.NextID++
	r := ScheduleRow{ID: id, Name: strings.TrimSpace(in.Name), Task: in.Task,
		IntervalMinutes: in.IntervalMinutes, Timing: t, Enabled: enabled, StopAfter: stop}
	if r.Name == "" {
		r.Name = scheduleTaskLabels[r.Task]
	}
	if t.Mode == "preset" {
		r.NextRunAt = scheduleStamp(time.Now())
	} else {
		r.NextRunAt = scheduleStamp(nextRun(r, time.Now()))
	}
	m.store.Rows = append(m.store.Rows, r)
	m.saveLocked()
	m.mu.Unlock()
	m.notify()
	return id, nil
}

func (m *ScheduleManager) patch(id int64, in scheduleInput) error {
	m.mu.Lock()
	idx := -1
	for i := range m.store.Rows {
		if m.store.Rows[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		m.mu.Unlock()
		return fmt.Errorf("任务不存在")
	}
	r := m.store.Rows[idx]
	wasEnabled := r.Enabled
	recalc := false
	if in.Name != "" {
		r.Name = strings.TrimSpace(in.Name)
	}
	if in.Task != "" {
		if _, ok := scheduleTaskLabels[in.Task]; !ok {
			m.mu.Unlock()
			return fmt.Errorf("未知任务类型")
		}
		r.Task = in.Task
		recalc = true
	}
	if in.IntervalMinutes > 0 {
		if in.IntervalMinutes > 5256000 {
			m.mu.Unlock()
			return fmt.Errorf("执行间隔超出范围")
		}
		r.IntervalMinutes = in.IntervalMinutes
		recalc = true
	}
	if in.Timing != nil {
		r.Timing = normalizeTiming(*in.Timing)
		recalc = true
	}
	if in.Enabled != nil {
		r.Enabled = *in.Enabled
	}
	if in.StopAfter != nil {
		r.StopAfter = strings.TrimSpace(*in.StopAfter)
	}
	if recalc || (r.Enabled && !wasEnabled) {
		r.NextRunAt = scheduleStamp(nextRun(r, time.Now()))
	}
	m.store.Rows[idx] = r
	m.saveLocked()
	m.mu.Unlock()
	m.notify()
	return nil
}

func (m *ScheduleManager) toggle(id int64) (bool, error) {
	m.mu.Lock()
	for i := range m.store.Rows {
		if m.store.Rows[i].ID == id {
			m.store.Rows[i].Enabled = !m.store.Rows[i].Enabled
			if m.store.Rows[i].Enabled {
				m.store.Rows[i].NextRunAt = scheduleStamp(nextRun(m.store.Rows[i], time.Now()))
			}
			enabled := m.store.Rows[i].Enabled
			m.saveLocked()
			m.mu.Unlock()
			m.notify()
			return enabled, nil
		}
	}
	m.mu.Unlock()
	return false, fmt.Errorf("任务不存在")
}

func (m *ScheduleManager) remove(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.store.Rows {
		if m.store.Rows[i].ID == id {
			m.store.Rows = append(m.store.Rows[:i], m.store.Rows[i+1:]...)
			m.saveLocked()
			m.notify()
			return nil
		}
	}
	return fmt.Errorf("任务不存在")
}

func (m *ScheduleManager) tryStart(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running[id] {
		return false
	}
	m.running[id] = true
	return true
}

func (m *ScheduleManager) finish(id int64) {
	m.mu.Lock()
	delete(m.running, id)
	m.mu.Unlock()
}

func scheduleErr(task, msg string) map[string]any {
	return map[string]any{"task": task, "ok": false, "error": msg, "summary": msg}
}

func dumpResult(v map[string]any) string {
	raw, err := json.Marshal(v)
	if len(raw) > 2000 {
		// 与参考 _dump_result 同量级上限；完整 JSON 仍合法，不截断半截。
		if s, ok := v["summary"].(string); ok && len(s) > 1200 {
			v["summary"] = s[:1200] + "…"
		}
		delete(v, "detail")
		delete(v, "errors")
		raw, err = json.Marshal(v)
	}
	if err != nil {
		return `{"ok":false,"error":"结果序列化失败"}`
	}
	return string(raw)
}

func (m *ScheduleManager) runTask(task string) map[string]any {
	p := m.panel
	result := map[string]any{"task": task, "ok": true}
	sch := p.cfg.Scheduler
	switch task {
	case "refresh_balances", "refresh_credits":
		if sch == nil {
			return scheduleErr(task, "调度器未装配")
		}
		sch.RunBalanceRefreshNow()
		result["summary"] = "已刷新全部账号余额与积分到期快照"
	case "sync_models":
		if p.cfg.Upstream == nil || p.cfg.Pool == nil {
			return scheduleErr(task, "上游客户端未装配")
		}
		a := p.cfg.Pool.PickForCatalog("")
		if a == nil {
			a = p.cfg.Pool.Pick()
		}
		if a == nil {
			return scheduleErr(task, "没有可用账号")
		}
		infos, err := p.cfg.Upstream.FetchModels(a)
		if err != nil {
			return scheduleErr(task, err.Error())
		}
		result["summary"] = fmt.Sprintf("已同步模型目录，共 %d 个模型", len(infos))
		result["total_in_db"] = len(infos)
	case "daily_checkin":
		if sch == nil {
			return scheduleErr(task, "调度器未装配")
		}
		sch.RunCheckinNow()
		result["summary"] = "签到与余额巡检已执行"
	case "refresh_growth_tasks":
		result["summary"] = "成长任务列表按请求实时读取，无需单独刷新"
	case "run_growth_tasks":
		if p.cfg.Pool == nil {
			return scheduleErr(task, "账号池未装配")
		}
		started, total, _, msg := p.startGrowthQueue(1, true, false)
		result["summary"] = fmt.Sprintf("%s；排队 %d 项", msg, total)
		result["running"] = started
	case "run_growth_plan":
		if p.gc == nil {
			return scheduleErr(task, "成长计划服务未装配")
		}
		started := p.gc.startPlanRound()
		result["summary"] = "成长计划正在执行，结果会自动刷新"
		if !started {
			result["summary"] = "成长计划正在执行或手动操作占锁，已跳过本轮"
		}
		result["running"] = started
	case "keepalive_tokens":
		if sch == nil {
			return scheduleErr(task, "调度器未装配")
		}
		sch.RunKeepaliveNow()
		result["summary"] = "token 保活巡检已执行"
	case "ban_recheck", "cooldown_probe":
		if sch == nil {
			return scheduleErr(task, "调度器未装配")
		}
		sch.RunCooldownProbeNow()
		result["summary"] = "冷却探活与账号复核已执行"
	case "travel":
		if sch == nil {
			return scheduleErr(task, "调度器未装配")
		}
		sch.RunTravelNow()
		result["summary"] = "猫猫旅行巡检已执行"
	case "activity":
		if sch == nil {
			return scheduleErr(task, "调度器未装配")
		}
		sch.RunActivityNow()
		result["summary"] = "每日活跃上报已执行"
	case "school":
		if sch == nil {
			return scheduleErr(task, "调度器未装配")
		}
		sch.RunSchoolNow()
		result["summary"] = "开学季活动已执行"
	case "streak_bonus":
		if sch == nil {
			return scheduleErr(task, "调度器未装配")
		}
		sch.RunStreakBonusNow()
		result["summary"] = "连登奖励与抽奖闭环已执行"
	case "blackcat":
		if sch == nil {
			return scheduleErr(task, "调度器未装配")
		}
		sch.RunBlackcatNow()
		result["summary"] = "夜猫子任务已执行"
	default:
		return scheduleErr(task, "未知任务类型")
	}
	return result
}

func (m *ScheduleManager) executeOnce(id int64, manual bool) (map[string]any, error) {
	r, ok := m.row(id)
	if !ok {
		return nil, fmt.Errorf("任务不存在")
	}
	if !m.tryStart(id) {
		return map[string]any{"ok": true, "running": true, "skipped": "任务正在执行"}, nil
	}
	now := time.Now().UTC()
	go func() {
		defer m.finish(id)
		out := map[string]any{}
		defer func() {
			if v := recover(); v != nil {
				out = scheduleErr(r.Task, fmt.Sprintf("任务异常：%v", v))
			}
			current, ok := m.row(id)
			if !ok {
				return
			}
			current.LastRunAt, current.LastResult = scheduleStamp(now), dumpResult(out)
			current.NextRunAt = scheduleStamp(nextRun(current, time.Now()))
			m.saveRow(current)
		}()
		out = m.runTask(r.Task)
	}()
	if manual {
		return map[string]any{"ok": true, "running": true, "summary": "任务已在后台启动，结果会自动刷新"}, nil
	}
	return map[string]any{"ok": true, "running": true}, nil
}

// executeScheduledSync 串行执行一个到期任务并同步回填结果。
// 为什么不用 executeOnce 的异步路径：参考 _loop 是逐条同步跑（成长计划除外），
// 若把同刻到期的签到/旅行/保活一起发 goroutine，Scheduler.beginRun 会让大部分
// 静默跳过，last_result 却仍写「已执行」——面板显示成功、实际没跑。
func (m *ScheduleManager) executeScheduledSync(r ScheduleRow, started time.Time) map[string]any {
	if !m.tryStart(r.ID) {
		return map[string]any{"ok": true, "running": true, "skipped": "任务正在执行"}
	}
	defer m.finish(r.ID)
	out := map[string]any{}
	defer func() {
		if v := recover(); v != nil {
			out = scheduleErr(r.Task, fmt.Sprintf("任务异常：%v", v))
		}
		current, ok := m.row(r.ID)
		if !ok {
			return
		}
		current.LastRunAt, current.LastResult = scheduleStamp(started), dumpResult(out)
		current.NextRunAt = scheduleStamp(nextRun(current, time.Now()))
		m.saveRow(current)
	}()
	out = m.runTask(r.Task)
	return out
}

func (m *ScheduleManager) Start(ctx context.Context) {
	m.once.Do(func() {
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			m.tick()
			for {
				select {
				case <-ctx.Done():
					return
				case <-m.wake:
					m.tick()
				case <-ticker.C:
					m.tick()
				}
			}
		}()
	})
}

func (m *ScheduleManager) tick() {
	now := time.Now().UTC()
	m.mu.Lock()
	rows := append([]ScheduleRow{}, m.store.Rows...)
	m.mu.Unlock()
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].Task == "run_growth_plan", rows[j].Task == "run_growth_plan"
		if a != b {
			return a
		}
		return rows[i].ID < rows[j].ID
	})
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		if stop := parseStamp(r.StopAfter); !stop.IsZero() && now.After(stop) {
			continue
		}
		next := parseStamp(r.NextRunAt)
		if !next.IsZero() && next.After(now) {
			continue
		}
		r.NextRunAt = scheduleStamp(nextRun(r, now))
		m.saveRow(r)
		if r.Task == "run_growth_plan" {
			// 成长计划单轮最多 3 分钟，独立后台跑（参考 start_growth_plan）。
			if _, err := m.executeOnce(r.ID, false); err != nil {
				log.Printf("schedule: 启动任务 %d(%s) 失败: %v", r.ID, r.Task, err)
			}
			continue
		}
		m.executeScheduledSync(r, now)
	}
}

func (p *Panel) schedulesReady(w http.ResponseWriter) *ScheduleManager {
	if p.sm == nil {
		writeErr(w, http.StatusServiceUnavailable, "定时任务服务未装配")
		return nil
	}
	return p.sm
}

func decodeScheduleBody(w http.ResponseWriter, r *http.Request) (scheduleInput, error) {
	var in scheduleInput
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in)
	return in, err
}

func (p *Panel) schedulesList(w http.ResponseWriter, r *http.Request) {
	if m := p.schedulesReady(w); m != nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": m.list()})
	}
}

func (p *Panel) schedulesCreate(w http.ResponseWriter, r *http.Request) {
	m := p.schedulesReady(w)
	if m == nil {
		return
	}
	in, err := decodeScheduleBody(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	id, err := m.create(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "ok": true})
}

func (p *Panel) schedulesPatch(w http.ResponseWriter, r *http.Request) {
	m := p.schedulesReady(w)
	if m == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "任务编号错误")
		return
	}
	in, err := decodeScheduleBody(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := m.patch(id, in); err != nil {
		// 参数校验错误（未知任务/间隔越界）回 400；只有 ID 不存在才回 404。
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "不存在") {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "ok": true})
}

func (p *Panel) schedulesToggle(w http.ResponseWriter, r *http.Request) {
	m := p.schedulesReady(w)
	if m == nil {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	enabled, err := m.toggle(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": enabled})
}

func (p *Panel) schedulesRun(w http.ResponseWriter, r *http.Request) {
	m := p.schedulesReady(w)
	if m == nil {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	res, err := m.executeOnce(id, true)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "result": res})
}

func (p *Panel) schedulesDelete(w http.ResponseWriter, r *http.Request) {
	m := p.schedulesReady(w)
	if m == nil {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := m.remove(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "ok": true})
}

func (p *Panel) schedulesStatus(w http.ResponseWriter, r *http.Request) {
	if m := p.schedulesReady(w); m != nil {
		m.mu.Lock()
		running := len(m.running) > 0
		m.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"items": m.list(), "running": running})
	}
}

// StartSchedules 启动定时任务轮询循环（main 装配后调用）。
func (p *Panel) StartSchedules(ctx context.Context) {
	if p.sm != nil {
		p.sm.Start(ctx)
	}
}

// growthPlanView 返回定时任务表里「WorkBuddy 成长计划巡检」的排程摘要，
// 供成长计划页底部状态行显示（参考前端 data.schedule）。找不到时返回 nil。
func (m *ScheduleManager) growthPlanView() map[string]any {
	m.mu.Lock()
	rows := append([]ScheduleRow{}, m.store.Rows...)
	m.mu.Unlock()
	for _, r := range rows {
		if r.Task != "run_growth_plan" {
			continue
		}
		return map[string]any{
			"id": r.ID, "enabled": r.Enabled,
			"interval_minutes": r.IntervalMinutes,
			"cycle_label":      cycleLabel(r), "next_run_at": nilIfEmpty(r.NextRunAt),
			"last_run_at": nilIfEmpty(r.LastRunAt), "last_result": r.LastResult,
		}
	}
	return nil
}
