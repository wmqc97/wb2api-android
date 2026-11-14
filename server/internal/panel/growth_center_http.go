// growth_center_http.go 成长计划的 HTTP 接口与自动巡检。
//
// 端点（都挂在 /panel/api/ 下，沿用 withAuth）：
//
//	GET  growth/accounts                 账号选择器
//	GET  growth/accounts/{uid}          快照 + 刷新状态 + 本地记录 + 巡检排程
//	POST growth/accounts/{uid}/refresh  启动后台只读刷新
//	GET  growth/accounts/{uid}/records  分页记录
//	GET  growth/accounts/{uid}/official 官方旅行/抽奖/领取记录
//	POST growth/accounts/{uid}/action   明确入口的单次动作
//	POST growth/run                     立即跑一轮巡检（异步）
//	GET  growth/status                  巡检进度
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func (p *Panel) growthReady(w http.ResponseWriter) *growthCenter {
	if p.gc == nil || p.cfg.Pool == nil || p.cfg.Upstream == nil {
		writeErr(w, http.StatusServiceUnavailable, "成长计划服务未装配")
		return nil
	}
	return p.gc
}

// growthAccountsHandler 账号选择器：返回支持个人成长体系的账号列表。
func (p *Panel) growthAccountsHandler(w http.ResponseWriter, r *http.Request) {
	if g := p.growthReady(w); g != nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": p.growthAccounts()})
	}
}

// growthOverview 快照秒回：不触发任何上游请求（刷新另有明确入口）。
func (p *Panel) growthOverview(w http.ResponseWriter, r *http.Request) {
	g := p.growthReady(w)
	if g == nil {
		return
	}
	uid := r.PathValue("uid")
	a, err := p.growthAccount(uid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	_ = a
	snap := g.growthSnap(uid)
	job := g.job(uid)
	job.Running = job.Running || snap.RefreshRunning
	rows, more, next := g.records(uid, 0, 20)
	latest := any(nil)
	if len(rows) > 0 {
		latest = rows[0]
	}
	g.mu.Lock()
	lastRun, lastResult := g.store.LastRunAt, g.store.LastResult
	g.mu.Unlock()
	// 排程以定时任务表中的 run_growth_plan 行为准：用户在「定时任务」页
	// 禁用/改周期/删除后，成长计划页的状态行与下次运行时刻立即一致。
	schedule := map[string]any{
		"enabled":          true,
		"interval_minutes": int(growthPlanInterval.Minutes()),
		"cycle_label":      "每 60 分钟",
		"next_run_at":      p.growthNextRun(),
		"last_run_at":      lastRun,
		"last_result":      lastResult,
	}
	if p.sm != nil {
		if view := p.sm.growthPlanView(); view != nil {
			schedule = view
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account": map[string]any{"uid": uid}, "snapshot": snap,
		"refresh": job, "latest_run": latest,
		"records": map[string]any{"items": rows, "has_more": more, "next_before": next},
		"today":   growthDay(time.Now()), "schedule": schedule,
	})
}

func (p *Panel) growthNextRun() string {
	if p.gc == nil {
		return ""
	}
	p.gc.mu.Lock()
	last := p.gc.store.LastRunAt
	p.gc.mu.Unlock()
	t, err := time.Parse(time.RFC3339, last)
	if err != nil || t.IsZero() {
		return time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	}
	return t.Add(growthPlanInterval).UTC().Format(time.RFC3339)
}

// growthRefresh 启动一次异步只读快照刷新。已在跑返回 200 running；槽满 429。
func (p *Panel) growthRefresh(w http.ResponseWriter, r *http.Request) {
	g := p.growthReady(w)
	if g == nil {
		return
	}
	uid := r.PathValue("uid")
	a, err := p.growthAccount(uid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if !g.startRefresh(uid, a) {
		writeErr(w, http.StatusTooManyRequests, "正在刷新其他账号，请稍后重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": true})
}

// growthRecords 分页读取本地执行/领取记录。
func (p *Panel) growthRecords(w http.ResponseWriter, r *http.Request) {
	g := p.growthReady(w)
	if g == nil {
		return
	}
	uid := r.PathValue("uid")
	if _, err := p.growthAccount(uid); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	rows, more, next := g.records(uid, before, limit)
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "has_more": more, "next_before": next})
}

// growthOfficial 官方领取记录：白名单投影，避免把物流地址/手机号带回面板。
func (p *Panel) growthOfficial(w http.ResponseWriter, r *http.Request) {
	g := p.growthReady(w)
	if g == nil {
		return
	}
	uid := r.PathValue("uid")
	a, err := p.growthAccount(uid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	kind := r.URL.Query().Get("kind")
	path, ok := growthOfficialPaths[kind]
	if !ok {
		writeErr(w, http.StatusBadRequest, "未知记录类型")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 1000 {
		page = 1000
	}
	res := p.cfg.Upstream.GrowthRequest(a, http.MethodGet,
		fmt.Sprintf("%s?page=%d&page_size=10", path, page), nil)
	if !res.OK {
		writeErr(w, http.StatusBadGateway, "官方领取记录暂时不可用，请稍后重试")
		return
	}
	data := res.Object()
	key := "items"
	if kind == "travel" {
		key = "records"
	}
	items := []any{}
	for _, row := range growthMapSlice(data[key]) {
		out := growthFields(row, "id", "record_id", "draw_id", "reward_id", "prize_name",
			"prize_type", "credit_amount", "reward_credit", "credit_granted", "energy_granted",
			"cards_granted", "chances_granted", "created_at", "drawn_at", "claimed_at",
			"acquired_at", "arrive_at", "depart_at", "travel_date", "status", "prize_code")
		out["location"] = growthFields(row["location"], "name", "code")
		out["prize"] = growthFields(row["prize"], "prize_name", "name", "prize_type", "type", "credit_amount")
		items = append(items, out)
	}
	total := growthNum(data["total"])
	if total == 0 {
		total = int64(len(items))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "page": page, "total": total, "page_size": 10,
	})
}

// growthAction 手动动作入口：先取 10 分钟租约，与自动巡检互斥。
func (p *Panel) growthAction(w http.ResponseWriter, r *http.Request) {
	g := p.growthReady(w)
	if g == nil {
		return
	}
	uid := r.PathValue("uid")
	a, err := p.growthAccount(uid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	var body struct{ Action, Tier string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body) != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.Action = strings.TrimSpace(body.Action)
	if _, ok := growthActionLabels[body.Action]; !ok {
		writeErr(w, http.StatusBadRequest, "未知成长操作")
		return
	}
	if !regexp.MustCompile("^[a-zA-Z0-9_-]*$").MatchString(body.Tier) {
		writeErr(w, http.StatusBadRequest, "奖励档位格式错误")
		return
	}
	for _, st := range p.cfg.Pool.List() {
		if st.UID == uid && st.Disabled {
			writeErr(w, http.StatusBadRequest, "请先启用账号或重新登录")
			return
		}
	}
	owner, ok := g.acquireLease()
	if !ok {
		writeErr(w, http.StatusConflict, "成长巡检正在执行，请等待完成后操作")
		return
	}
	defer g.releaseLease(owner)
	res, err := g.growthPerform(uid, a, body.Action, body.Tier)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// growthRun 立即开始一轮巡检（非阻塞，已在跑返回 running=true）。
func (p *Panel) growthRun(w http.ResponseWriter, r *http.Request) {
	g := p.growthReady(w)
	if g == nil {
		return
	}
	started := g.startPlanRound()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": started,
		"message": map[bool]string{true: "成长计划巡检已开始", false: "成长计划正在执行"}[started]})
}

// growthStatus 巡检状态（前端轮询用）。
func (p *Panel) growthStatus(w http.ResponseWriter, r *http.Request) {
	g := p.growthReady(w)
	if g == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"running": g.planRunning(),
		"next_run_at": p.growthNextRun()})
}

// startPlanRound 尝试启动巡检 goroutine；并发或手动动作占着租约时返回 false。
// 租约在启动前获取：否则前端会收到「已开始」，后台却因拿不到租约立刻退出。
func (g *growthCenter) startPlanRound() bool {
	g.runMutex.Lock()
	if g.running {
		g.runMutex.Unlock()
		return false
	}
	owner, ok := g.acquireLease()
	if !ok {
		g.runMutex.Unlock()
		return false
	}
	g.running = true
	g.runMutex.Unlock()
	go func() {
		defer func() {
			g.runMutex.Lock()
			g.running = false
			g.runMutex.Unlock()
		}()
		g.runPlanRoundWithLease(owner)
	}()
	return true
}

func (g *growthCenter) planRunning() bool {
	g.runMutex.Lock()
	defer g.runMutex.Unlock()
	return g.running
}

// runPlanRound 供测试/直接调用：自行取租约后执行一轮。
func (g *growthCenter) runPlanRound() {
	owner, ok := g.acquireLease()
	if !ok {
		return
	}
	g.runPlanRoundWithLease(owner)
}

// runPlanRoundWithLease 一轮最多 3 分钟，游标轮转：尾部账号不会因前面慢号永远漏跑。
// owner 由调用方取得，本函数负责释放（进程崩溃则靠 10 分钟过期自动恢复）。
func (g *growthCenter) runPlanRoundWithLease(owner string) {
	defer g.releaseLease(owner)
	started := time.Now()
	deadline := started.Add(growthPlanBudget)
	day := growthDay(started)
	all := []string{}
	g.mu.Lock()
	cursor := g.store.Cursor.ID
	remaining := append([]string{}, g.store.Cursor.Remaining...)
	g.mu.Unlock()
	for _, st := range g.panel.cfg.Pool.List() {
		if st.Disabled || st.Enterprise || st.Realm == "global" {
			continue
		}
		all = append(all, st.UID)
	}
	sort.Strings(all)
	queue := []string{}
	if len(remaining) > 0 {
		valid := map[string]bool{}
		for _, uid := range all {
			valid[uid] = true
		}
		for _, uid := range remaining {
			if valid[uid] {
				queue = append(queue, uid)
			}
		}
	} else {
		for _, uid := range all {
			if uid > cursor {
				queue = append(queue, uid)
			}
		}
		for _, uid := range all {
			if uid <= cursor {
				queue = append(queue, uid)
			}
		}
	}
	credit, failed := int64(0), 0
	processed := 0
	summary := ""
	for len(queue) > 0 {
		if time.Now().After(deadline) {
			break
		}
		if !g.renewLease(owner) {
			break
		}
		uid := queue[0]
		queue = queue[1:]
		a, err := g.panel.growthAccount(uid)
		if err != nil {
			continue
		}
		st := g.state(uid)
		if st.Identity == "" {
			st.Identity = growthIdentity(uid)
		}
		res := g.inspectAccount(uid, a, &st, day, func() bool { return time.Now().Before(deadline) })
		g.saveState(uid, &st)
		g.record(uid, "schedule", "成长计划巡检", res)
		processed++
		credit += res.Credit
		if !res.OK {
			failed++
		}
		g.mu.Lock()
		g.store.Cursor.ID, g.store.Cursor.Remaining = uid, append([]string{}, queue...)
		g.saveLocked()
		g.mu.Unlock()
		if res.Credit > 0 {
			g.syncBalance(uid, a)
		}
	}
	summary = fmt.Sprintf("成长计划已检查 %d/%d 个账号，领取 %d 积分", processed, len(all), credit)
	if failed > 0 {
		summary += fmt.Sprintf("，%d 个需复查", failed)
	}
	if len(queue) > 0 {
		summary += fmt.Sprintf("，剩余 %d 个将在下轮继续", len(queue))
	}
	g.mutate(func(s *growthStore) {
		s.LastRunAt, s.LastResult = growthStamp(), summary
		s.Cursor.Remaining = append([]string{}, queue...)
	})
}

// inspectAccount 单账号巡检。所有写动作按 growthAccountState 做自然日幂等；
// call 包一层时间预算与租约续期，失败按软结果记步骤而不中断后续账号。
func (g *growthCenter) inspectAccount(uid string, a *auth.Auth, st *growthAccountState, day string, live func() bool) growthResult {
	up := g.panel.cfg.Upstream
	steps := []growthStep{}
	credit := int64(0)
	add := func(name string, ok bool, message string, reward int64) {
		if reward > 0 {
			credit += reward
		}
		steps = append(steps, growthStep{Step: name, OK: ok, Message: message, Reward: reward})
	}
	save := func() { g.saveState(uid, st) }
	request := func(name string, method, path string, body any) (upstream.SoftResult, bool) {
		if !live() {
			add(name, false, "本轮时间预算已到，下轮继续检查", 0)
			return upstream.SoftResult{}, false
		}
		res := up.GrowthRequest(a, method, path, body)
		if !res.OK {
			add(name, false, fmt.Sprintf("接口失败 HTTP %d / code %d", res.Status, res.Code), 0)
			return res, false
		}
		return res, true
	}
	// ① 每日活跃：热力图已点亮就跳过对话；否则每号每天最多一次上报。
	heat, ok := request("每日活跃", http.MethodGet, "/activity/growth/heatmap", nil)
	if ok {
		g.observe(uid, "heatmap", heat.Object(), false)
		today, _ := heat.Object()["today"].(map[string]any)
		active := growthStr(today["date"]) == day && growthBool(today["is_active"])
		if active {
			st.ActiveDay = day
			save()
			add("每日活跃", true, "今日热力图已点亮，跳过对话", 0)
		} else {
			if st.ChatDay != day {
				attempt := st.ChatAttemptAt
				if time.Now().Unix()-attempt >= int64(growthChatRetryGap.Seconds()) {
					st.ChatAttemptAt = time.Now().Unix()
					save() // 发请求前落库：超时/崩溃后不会立刻重发
					err := up.ReportChatActivity(a, fmt.Sprintf("wb2api-growth-%d", time.Now().UnixMilli()), "")
					if err == nil {
						st.ChatDay = day
						save()
					} else {
						add("每日活跃对话", false, "免费模型对话未完成，一小时后重试", 0)
					}
				} else {
					add("每日活跃对话", false, "上次请求失败或结果未知，一小时后重试", 0)
				}
			}
			if st.ChatDay == day {
				confirmed := false
				for i, delay := range []time.Duration{0, 500 * time.Millisecond, time.Second} {
					if delay > 0 {
						time.Sleep(delay)
					}
					h := up.GrowthRequest(a, http.MethodGet, "/activity/growth/heatmap", nil)
					if h.OK {
						t, _ := h.Object()["today"].(map[string]any)
						if growthStr(t["date"]) == day && growthBool(t["is_active"]) {
							confirmed = true
							st.ActiveDay = day
							save()
							g.observe(uid, "heatmap", h.Object(), false)
							break
						}
					}
					_ = i
				}
				if confirmed {
					add("每日活跃", true, "对话已完成，热力图已点亮", 0)
				} else {
					add("每日活跃", false, "对话已完成，活跃尚未入账；下轮只查状态", 0)
				}
			}
		}
	}
	// ② 每日打卡（与热力图活跃是两套独立活动）。
	if st.CheckinDay != day {
		status := up.CheckinStatus(a)
		obj := status.Object()
		g.observe(uid, "checkin", obj, false)
		switch {
		case status.OK && growthBool(obj["today_checked_in"]):
			st.CheckinDay = day
			save()
			add("每日打卡", true, "今日积分已领", 0)
		case status.OK && obj["active"] == false:
			add("每日打卡", true, "签到活动未开启或已结束", 0)
		case status.OK && growthBool(obj["active"]):
			res := up.ClaimDailyCheckinSoft(a)
			already := strings.Contains(strings.ToLower(res.Msg), "already")
			if res.OK || already {
				st.CheckinDay = day
				save()
				msg := "签到积分已领取"
				if already {
					msg = "今日积分已领"
				}
				reward := int64(0)
				if res.OK {
					reward = growthNum(res.Object()["credit"])
				}
				add("每日打卡", true, msg, reward)
			} else {
				add("每日打卡", false, "未领取，下轮重新检查签到状态", 0)
			}
		default:
			add("每日打卡", false, "签到状态不可用", 0)
		}
	} else {
		add("每日打卡", true, "今日积分已领", 0)
	}
	// ③ 宠物领养 / 旅行礼物。
	buddy, ok := request("当前宠物", http.MethodGet, "/activity/growth/buddy/info", nil)
	if ok {
		g.observe(uid, "buddy", buddy.Object(), false)
		if growthFields(buddy.Object()["buddy"], "name")["name"] == nil {
			if _, ok := request("宠物协议", http.MethodPost, "/activity/growth/buddy/agreement", map[string]any{"agree": true}); ok {
				if _, ok := request("首次领养", http.MethodPost, "/activity/growth/buddy/first", map[string]any{}); ok {
					add("首次领养", true, "首次宠物已领取", 0)
					if b := up.GrowthRequest(a, http.MethodGet, "/activity/growth/buddy/info", nil); b.OK {
						g.observe(uid, "buddy", b.Object(), false)
					}
				}
			}
		}
		if travel, ok := request("旅行礼物", http.MethodGet, "/activity/growth/buddy/travel/status", nil); ok {
			tobj := travel.Object()
			g.observe(uid, "travel", tobj, false)
			state := growthStr(tobj["state"])
			if state == "arrived" {
				if gift, ok := request("领取礼物", http.MethodPost, "/activity/growth/buddy/travel/claim", map[string]any{}); ok {
					add("领取礼物", true, "旅行礼物已领取", growthNum(gift.Object()["reward_credit"]))
				}
			} else if state == "idle" && !growthBool(tobj["daily_limit_reached"]) {
				loc := 4
				if cfg, ok := request("旅行地点", http.MethodGet, "/activity/growth/buddy/travel/config", nil); ok {
					for _, v := range growthMapSlice(cfg.Object()["locations"]) {
						if n := growthNum(v["location_id"]); n > 0 {
							loc = int(n)
							break
						}
						if n := growthNum(v["id"]); n > 0 {
							loc = int(n)
							break
						}
						codes := map[string]int{"coffee": 1, "mall": 2, "gym": 3, "ancient_town": 4}
						if n := codes[growthStr(v["code"])]; n > 0 {
							loc = n
							break
						}
					}
				}
				if _, ok := request("派出旅行", http.MethodPost, "/activity/growth/buddy/travel/depart", map[string]any{"location_id": loc}); ok {
					add("派出旅行", true, "已派出，下轮到站自动领礼物", 0)
				}
			} else if state == "traveling" {
				add("领取礼物", true, "旅行中，到站后领取", 0)
			} else {
				add("派出旅行", true, "已达到今日旅行上限", 0)
			}
		}
	}
	// ④ 连登档位兑换（每月每档一次，token 按身份+月份+档位稳定派生）。
	if streak, ok := request("连登奖励", http.MethodGet, "/activity/growth/streak", nil); ok {
		sobj := streak.Object()
		g.observe(uid, "streak", sobj, false)
		rs, _ := sobj["redemption_status"].(map[string]any)
		eligible := 0
		for _, t := range growthMapSlice(rs["tiers"]) {
			tier := growthStr(t["tier"])
			if tier == "" || growthStr(rs["tier_"+tier+"_status"]) != "available" {
				continue
			}
			key := day[:7] + ":" + tier
			if st.Redeemed[key] != "" {
				continue
			}
			token := "redeem-" + growthIdentity(st.Identity+":"+day[:7]+":"+tier)
			res, ok := request("兑换"+tier+"奖励", http.MethodPost, "/activity/growth/redeem",
				map[string]any{"tier": tier, "client_token": token})
			if !ok {
				continue
			}
			eligible++
			st.Redeemed[key] = day
			save()
			msg := "已领取连登奖励"
			if n := growthNum(res.Object()["cards_overflow"]); n > 0 {
				msg += fmt.Sprintf("，补登卡达上限，溢出 %d 张", n)
			}
			add("兑换"+tier+"奖励", true, msg, growthNum(res.Object()["credit_granted"]))
		}
		if eligible == 0 {
			add("连登奖励", true, "暂无可兑换档位（未达标或本月已领）", 0)
		}
	}
	// ⑤ 限时抽奖：pending token 跨天保留，409 duplicate 视为已处理。
	if lottery, ok := request("限时抽奖", http.MethodGet, "/activity/growth/lottery/summary", nil); ok {
		lobj := lottery.Object()
		g.observe(uid, "lottery", lobj, false)
		if growthBool(growthFields(lobj["module"], "enabled")["enabled"]) {
			if st.DrawDay != day {
				st.DrawDay, st.DrawCount = day, 0
				save()
			}
			available := int(growthNum(lobj["chances"]))
			if chances, ok := request("抽奖次数", http.MethodGet, "/activity/growth/lottery/chances", nil); ok {
				available = int(growthNum(chances.Object()["balance"]))
			}
			pending := 0
			if st.DrawToken != "" {
				pending = 1
			}
			attempts := available + pending
			if left := growthMaxDrawsPerDay - st.DrawCount; attempts > left {
				attempts = left
			}
			if attempts < 0 {
				attempts = 0
			}
			for i := 0; i < attempts; i++ {
				if !live() {
					break
				}
				if st.DrawToken == "" {
					st.DrawToken = fmt.Sprintf("draw-%d-%d", time.Now().UnixNano(), i)
				}
				save()
				res := up.GrowthRequest(a, http.MethodPost, "/activity/growth/lottery/draw",
					map[string]any{"client_token": st.DrawToken})
				msgLower := strings.ToLower(res.Msg)
				if !res.OK {
					if res.Status == http.StatusConflict && strings.Contains(msgLower, "duplicate") {
						st.DrawToken, st.DrawCount = "", st.DrawCount+1
						save()
						add("限时抽奖", true, "此前抽奖已处理，避免重复计奖", 0)
						continue
					}
					if strings.Contains(msgLower, "insufficient lottery chance balance") || strings.Contains(msgLower, "lottery disabled") {
						st.DrawToken = ""
						save()
					}
					add("限时抽奖", false, "抽奖未确认，下轮使用同一 token 核对", 0)
					break
				}
				st.DrawToken, st.DrawCount = "", st.DrawCount+1
				save()
				add("限时抽奖", true, "抽奖已完成；实物奖品请在官方成长中心填写领取信息", growthNum(res.Object()["credit_granted"]))
			}
			if attempts == 0 {
				add("限时抽奖", true, "暂无免费次数或已达每日自动抽奖上限（5次）", 0)
			}
		} else {
			add("限时抽奖", true, "限时抽奖未开启", 0)
		}
	}
	// ⑥ 盲盒：一天最多一批，发请求前记账，未知结果不重试消耗能量。
	if quota, ok := request("盲盒额度", http.MethodGet, "/activity/growth/buddy/quota", nil); ok {
		qobj := quota.Object()
		g.observe(uid, "quota", qobj, false)
		g.observe(uid, "energy", map[string]any{"balance": qobj["balance"]}, false)
		count := int(growthNum(qobj["affordable"]))
		if max := int(growthNum(qobj["max_open_count"])); max > 0 && count > max {
			count = max
		}
		if count > growthMaxBoxes {
			count = growthMaxBoxes
		}
		switch {
		case st.BoxAttemptDay == day:
			confirmed := st.BoxConfirmedDay == day
			msg := "今日已完成开盒；避免重复消耗能量"
			if !confirmed {
				msg = "今日开盒结果未知，避免重复消耗；请在官方宠物列表核对"
			}
			add("开启盲盒", confirmed, msg, 0)
		case count < 1:
			add("开启盲盒", true, "能量不足，保留至下轮检查", 0)
		default:
			st.BoxAttemptDay = day
			save()
			if boxes, ok := request("开启盲盒", http.MethodPost, "/activity/growth/buddy/open", map[string]any{"count": count}); ok {
				st.BoxConfirmedDay = day
				save()
				add("开启盲盒", true, fmt.Sprintf("已开启 %d 个盲盒", len(growthMapSlice(boxes.Object()["results"]))), 0)
				_, _ = request("开盒后额度", http.MethodGet, "/activity/growth/buddy/quota", nil)
			}
		}
	}
	okAll := len(steps) > 0
	for _, s := range steps {
		okAll = okAll && s.OK
	}
	msgParts := []string{}
	for _, s := range steps {
		msgParts = append(msgParts, s.Step+"："+s.Message)
	}
	return growthResult{OK: okAll, Credit: credit, Reason: strings.Join(msgParts, "；"), Steps: steps}
}

// StartGrowthPlan 启动每小时自动巡检循环（服务启动后 5 分钟跑首轮）。
// 与 Scheduler 的每日「成长任务队列」互不冲突：那条跑任务码，这里跑成长中心
// 的签到/旅行/连登/抽奖/盲盒闭环，都是明确入口的幂等动作。
func (p *Panel) StartGrowthPlan(ctx context.Context) {
	if p.gc == nil {
		return
	}
	go func() {
		timer := time.NewTimer(5 * time.Minute)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		_ = p.gc.startPlanRound()
		ticker := time.NewTicker(growthPlanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = p.gc.startPlanRound()
			}
		}
	}()
}
