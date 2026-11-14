// growth_center.go Go 版「成长计划」（原参考项目 admin/growth_plan.py +
// admin/growth_center.py + admin/routers/growth_center.py）。
//
// 与 Python 参考的对应：
//   - SystemSetting 快照/进度/租约 → App 私有目录 data/growth_plan.json 单文件；
//   - GrowthPlanRecord → 同文件的 records 数组（逐账号有界保留）；
//   - _Lease → 进程内互斥 + 文件里的 10 分钟过期租约（崩溃后可恢复）；
//   - run_growth_plan → 后台 goroutine，每小时一轮、单轮 3 分钟预算、游标轮转；
//   - /api/growth-center/* → /panel/api/growth/*，沿用面板 Bearer 鉴权。
//
// 设计约束：
//   - 读取页面绝不触发对话/领奖；刷新快照只打只读端点；
//   - 进度按账号 uid 的 SHA-256 派生身份存盘，不存 token / 邮箱 / 物流地址；
//   - 企业号 / 国际号没有个人成长体系，一律门控跳过；
//   - 严格 CSP（script-src 'self'）：前端只经 JS 事件委托绑 onclick，不写内联处理器。
package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// growthDayTZ 成长中心「自然日」口径：上游以 Asia/Shanghai 计算。
var growthDayTZ = time.FixedZone("CST", 8*3600)

const (
	growthPlanInterval   = time.Hour
	growthPlanBudget     = 3 * time.Minute
	growthPlanLeaseTTL   = 10 * time.Minute
	growthRefreshTTL     = 4 * time.Minute
	growthMaxDrawsPerDay = 5
	growthMaxBoxes       = 5
	growthMaxRecords     = 400
	growthChatRetryGap   = time.Hour
)

// growthSection 快照分项：脱敏后的 data + 更新时间；合并时按时间取新。
type growthSection struct {
	Data      map[string]any `json:"data"`
	UpdatedAt string         `json:"updated_at"`
}

// growthSnapshot 单账号快照。Sections 的 key 与参考前端 section() 对齐。
type growthSnapshot struct {
	Sections       map[string]growthSection `json:"sections"`
	Errors         map[string]string        `json:"errors"`
	UpdatedAt      string                   `json:"updated_at"`
	CheckedAt      string                   `json:"checked_at"`
	RefreshRunning bool                     `json:"refresh_running"`
	ExpiresAt      int64                    `json:"refresh_expires_at"`
}

// growthRecord 一轮巡检 / 一次手动操作的本地记录（取代 Python 的 DB 行）。
type growthRecord struct {
	ID        int64        `json:"id"`
	UID       string       `json:"uid"`
	Source    string       `json:"source"` // schedule / manual
	Action    string       `json:"action"`
	OK        bool         `json:"ok"`
	Credit    int64        `json:"credit"`
	CreatedAt string       `json:"created_at"`
	Message   string       `json:"message"`
	Steps     []growthStep `json:"steps"`
}

// growthStep 单步执行明细。
type growthStep struct {
	Step    string `json:"step"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Reward  int64  `json:"reward"`
}

// growthResult 一轮巡检/动作结果（与前端协议一致）。
type growthResult struct {
	OK      bool         `json:"ok"`
	Credit  int64        `json:"credit"`
	Reason  string       `json:"reason"`
	Steps   []growthStep `json:"steps"`
	Skipped string       `json:"skipped,omitempty"`
}

// growthAccountState 单账号幂等台账（按自然日 / pending token 防重复消耗）。
type growthAccountState struct {
	Identity        string            `json:"identity"`
	ActiveDay       string            `json:"active_day"`
	ChatDay         string            `json:"chat_day"`
	ChatAttemptAt   int64             `json:"chat_attempt_at"`
	CheckinDay      string            `json:"checkin_day"`
	BoxAttemptDay   string            `json:"box_attempt_day"`
	BoxConfirmedDay string            `json:"box_confirmed_day"`
	DrawDay         string            `json:"draw_day"`
	DrawCount       int               `json:"draw_count"`
	DrawToken       string            `json:"draw_token"`
	Redeemed        map[string]string `json:"redeemed"`
}

// growthStore 落盘的完整状态。
type growthStore struct {
	Snapshots map[string]*growthSnapshot     `json:"snapshots"`
	States    map[string]*growthAccountState `json:"states"`
	Records   []growthRecord                 `json:"records"`
	NextID    int64                          `json:"next_id"`
	Lease     struct {
		Owner   string `json:"owner"`
		Expires int64  `json:"expires"`
	} `json:"lease"`
	Cursor struct {
		ID        string   `json:"id"`
		Remaining []string `json:"remaining"`
	} `json:"cursor"`
	LastRunAt  string `json:"last_run_at"`
	LastResult string `json:"last_result"`
}

// growthCenter 成长计划后端：状态、刷新任务与单轮巡检互斥都在此。
type growthCenter struct {
	mu       sync.Mutex
	path     string
	store    growthStore
	jobs     map[string]*growthJob
	refresh  chan struct{}
	runMutex sync.Mutex
	running  bool
	panel    *Panel
}

// growthJob 一次后台刷新：running / 完成时间 / 错误。
type growthJob struct {
	Running    bool   `json:"running"`
	FinishedAt int64  `json:"finished_at"`
	Error      string `json:"error"`
}

// growthSections 快照端点表（顺序固定，逐项保存，前半段失败不丢后半段）。
var growthSections = []struct{ Name, Label, Path string }{
	{"heatmap", "全年活跃", "/activity/growth/heatmap"},
	{"streak", "连续登录", "/activity/growth/streak"},
	{"energy", "能量", "/activity/growth/energy"},
	{"quota", "盲盒额度", "/activity/growth/buddy/quota"},
	{"buddy", "当前宠物", "/activity/growth/buddy/info"},
	{"travel", "旅行礼物", "/activity/growth/buddy/travel/status"},
	{"lottery", "抽奖次数", "/activity/growth/lottery/summary"},
	{"redeem", "兑换统计", "/activity/growth/redeem/summary"},
	{"buddies", "宠物图鉴", "/activity/growth/buddy/list"},
	{"profile", "成长等级", "/v2/activity/growth/profile"},
}

var growthOfficialPaths = map[string]string{
	"travel":  "/activity/growth/buddy/travel/records",
	"draws":   "/activity/growth/lottery/draws",
	"rewards": "/activity/growth/lottery/rewards",
}

func growthStamp() string            { return time.Now().UTC().Format(time.RFC3339) }
func growthDay(now time.Time) string { return now.In(growthDayTZ).Format("2006-01-02") }
func growthIdentity(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(sum[:])[:32]
}

func newGrowthStore() growthStore {
	return growthStore{
		Snapshots: map[string]*growthSnapshot{},
		States:    map[string]*growthAccountState{},
		Records:   []growthRecord{},
		NextID:    1,
	}
}

// newGrowthCenter 构造成长中心。stateFile 与 config.json / state.json 同目录；
// 传空时用当前目录（单测/无状态场景仅在内存工作）。
func newGrowthCenter(p *Panel, stateFile string) *growthCenter {
	name := "growth_plan.json"
	dir := filepath.Dir(stateFile)
	if dir != "" && dir != "." {
		name = filepath.Join(dir, name)
	}
	g := &growthCenter{
		path:    name,
		store:   newGrowthStore(),
		jobs:    map[string]*growthJob{},
		refresh: make(chan struct{}, 2), // 最多 2 个并发刷新槽（同参考 BoundedSemaphore(2)）
		panel:   p,
	}
	g.load()
	return g
}

func (g *growthCenter) load() {
	raw, err := os.ReadFile(g.path)
	if err != nil {
		return
	}
	var s growthStore
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	if s.Snapshots == nil {
		s.Snapshots = map[string]*growthSnapshot{}
	}
	if s.States == nil {
		s.States = map[string]*growthAccountState{}
	}
	if s.Records == nil {
		s.Records = []growthRecord{}
	}
	if s.NextID < 1 {
		s.NextID = 1
	}
	g.store = s
}

// mutate 在锁内改内存并原子落盘。fn 不得发网络请求。
func (g *growthCenter) mutate(fn func(*growthStore)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fn(&g.store)
	g.saveLocked()
}

func (g *growthCenter) saveLocked() {
	raw, err := json.Marshal(g.store)
	if err != nil {
		return
	}
	dir := filepath.Dir(g.path)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	tmp := g.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, g.path)
}

// account 按 uid 找池内账号并做成长体系门控（企业号/global 一律无）。
func (p *Panel) growthAccount(uid string) (*auth.Auth, error) {
	if p.cfg.Pool == nil || p.cfg.Upstream == nil {
		return nil, errors.New("成长计划服务未就绪")
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return nil, errors.New("账号不存在")
	}
	if a.IsEnterprise() {
		return nil, errors.New("企业账号不支持个人成长计划")
	}
	if a.IsGlobal() {
		return nil, errors.New("国际版账号不支持个人成长计划")
	}
	return a, nil
}

func (p *Panel) growthAccounts() []map[string]any {
	out := []map[string]any{}
	if p.cfg.Pool == nil || p.gc == nil {
		return out
	}
	p.gc.mu.Lock()
	defer p.gc.mu.Unlock()
	for _, st := range p.cfg.Pool.List() {
		if st.Enterprise || st.Realm == "global" {
			continue // 上游注定 403，不在账号选择器里给假入口
		}
		snap := p.gc.store.Snapshots[st.UID]
		var streakDays, energy any
		if snap != nil {
			if sec := snap.Sections["streak"]; sec.Data != nil {
				if m, ok := sec.Data["streak"].(map[string]any); ok {
					streakDays = m["days"]
				}
			}
			if sec := snap.Sections["energy"]; sec.Data != nil {
				energy = sec.Data["balance"]
			}
		}
		name := st.Nickname
		if name == "" {
			name = st.UID
		}
		out = append(out, map[string]any{
			"uid": st.UID, "name": name, "disabled": st.Disabled,
			"streak_days": streakDays, "energy": energy,
		})
	}
	return out
}

// growthSnap 返回快照副本（避免 handler 持锁渲染时撞上后台刷新写）。
func (g *growthCenter) growthSnap(uid string) growthSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return cloneSnapshot(g.store.Snapshots[uid])
}

func cloneSnapshot(s *growthSnapshot) growthSnapshot {
	out := growthSnapshot{
		Sections: map[string]growthSection{}, Errors: map[string]string{},
	}
	if s == nil {
		return out
	}
	for k, v := range s.Sections {
		data := map[string]any{}
		if raw, err := json.Marshal(v.Data); err == nil {
			_ = json.Unmarshal(raw, &data)
		}
		if data == nil {
			data = map[string]any{}
		}
		out.Sections[k] = growthSection{Data: data, UpdatedAt: v.UpdatedAt}
	}
	for k, v := range s.Errors {
		out.Errors[k] = v
	}
	out.UpdatedAt, out.CheckedAt = s.UpdatedAt, s.CheckedAt
	out.RefreshRunning = s.RefreshRunning && s.ExpiresAt > time.Now().Unix()
	out.ExpiresAt = s.ExpiresAt
	return out
}

func cloneState(s *growthAccountState) growthAccountState {
	out := growthAccountState{Redeemed: map[string]string{}}
	if s == nil {
		return out
	}
	out = *s
	out.Redeemed = map[string]string{}
	for k, v := range s.Redeemed {
		out.Redeemed[k] = v
	}
	return out
}

// growthNum 宽容解析上游数字（float64/json.Number/string 都可能出现）。
func growthNum(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func growthBool(v any) bool { return v == true || v == "true" }

func growthStr(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// growthFields 白名单投影（与参考 clean_section 同义）：绝不把邮箱、
// 物流地址或凭证带进快照/记录。
func growthFields(src any, names ...string) map[string]any {
	out := map[string]any{}
	m, _ := src.(map[string]any)
	for _, name := range names {
		if m != nil {
			if v, ok := m[name]; ok {
				out[name] = v
			}
		}
	}
	return out
}

func growthClean(name string, value any) map[string]any {
	m, _ := value.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	switch name {
	case "heatmap":
		cells := []any{}
		for _, v := range growthMapSlice(m["cells"]) {
			cells = append(cells, growthFields(v, "date", "score", "has_new_buddy"))
		}
		return map[string]any{
			"cells":      cells,
			"today":      growthFields(m["today"], "date", "score", "is_active", "status_text"),
			"range":      growthFields(m["range"], "start_date", "end_date", "days"),
			"updated_at": growthStr(m["updated_at"]),
		}
	case "streak":
		rs, _ := m["redemption_status"].(map[string]any)
		if rs == nil {
			rs = map[string]any{}
		}
		red := growthFields(rs, "tier_7d_status", "tier_14d_status", "tier_28d_status",
			"tier_7d_count", "tier_14d_count", "tier_28d_count", "remaining_days")
		tiers := []any{}
		for _, v := range growthMapSlice(rs["tiers"]) {
			tiers = append(tiers, growthFields(v, "tier", "days", "credit", "energy", "cards", "chances"))
		}
		red["tiers"] = tiers
		return map[string]any{
			"streak":            growthFields(m["streak"], "days", "month_total_days", "next_tier", "next_tier_remaining", "makeup_dates"),
			"redemption_status": red,
			"makeup_cards":      growthFields(m["makeup_cards"], "balance", "max"),
			"timezone":          growthStr(m["timezone"]),
		}
	case "buddy":
		return map[string]any{"buddy": growthFields(m["buddy"], "instance_id", "template_id", "name", "personality", "rarity", "thumbnail_url", "base_static_url")}
	case "buddies":
		list := []any{}
		for _, v := range growthMapSlice(m["buddies"]) {
			list = append(list, growthFields(v, "instance_id", "name", "personality", "rarity", "thumbnail_url", "base_static_url", "acquired_at", "source", "current_buddy"))
		}
		return map[string]any{"buddies": list, "count": growthNum(m["count"])}
	case "travel":
		out := growthFields(m, "state", "depart_at", "arrive_at", "server_now", "daily_limit_reached", "reward_credit")
		out["location"] = growthFields(m["location"], "id", "code", "name", "duration_hours")
		return out
	case "lottery":
		return map[string]any{"chances": growthNum(m["chances"]), "module": growthFields(m["module"], "enabled")}
	default:
		cols := map[string][]string{
			"energy":  {"balance", "total_consumed", "total_earned"},
			"quota":   {"balance", "affordable", "cost_per_open", "max_open_count"},
			"profile": {"level", "level_name", "completed", "total", "max_level"},
			"redeem":  {"starter_count", "advanced_count", "legendary_count", "remaining_days", "month_total_days"},
			"checkin": {"active", "today_checked_in", "daily_credit", "today_credit", "streak_days", "end_time"},
		}
		return growthFields(m, cols[name]...)
	}
}

func growthMapSlice(v any) []map[string]any {
	out := []map[string]any{}
	arr, _ := v.([]any)
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// observe 把一个成功的分项写入快照（逐项落盘，刷新中途失败不丢已取数据）。
func (g *growthCenter) observe(uid, name string, data any, save bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.store.Snapshots[uid]
	if s == nil {
		s = &growthSnapshot{}
		g.store.Snapshots[uid] = s
	}
	if s.Sections == nil {
		s.Sections = map[string]growthSection{}
	}
	if s.Errors == nil {
		s.Errors = map[string]string{}
	}
	now := growthStamp()
	s.Sections[name] = growthSection{Data: growthClean(name, data), UpdatedAt: now}
	delete(s.Errors, name)
	s.UpdatedAt = now
	if save {
		g.saveLocked()
	}
}

func (g *growthCenter) observeError(uid, name, message string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.store.Snapshots[uid]
	if s == nil {
		s = &growthSnapshot{}
		g.store.Snapshots[uid] = s
	}
	if s.Errors == nil {
		s.Errors = map[string]string{}
	}
	if s.Sections == nil {
		s.Sections = map[string]growthSection{}
	}
	s.Errors[name] = message
	g.saveLocked()
}

// record 追加一条本地记录（保留上限，按 uid 过滤供前端分页）。
func (g *growthCenter) record(uid, source, action string, res growthResult) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := growthRecord{
		ID: g.store.NextID, UID: uid, Source: source, Action: action,
		OK: res.OK, Credit: res.Credit, CreatedAt: growthStamp(),
		Message: res.Reason, Steps: res.Steps,
	}
	if r.Steps == nil {
		r.Steps = []growthStep{}
	}
	g.store.NextID++
	g.store.Records = append(g.store.Records, r)
	if len(g.store.Records) > growthMaxRecords {
		g.store.Records = append([]growthRecord{}, g.store.Records[len(g.store.Records)-growthMaxRecords:]...)
	}
	g.saveLocked()
}

func (g *growthCenter) records(uid string, before int64, limit int) ([]growthRecord, bool, int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rows := []growthRecord{}
	for i := len(g.store.Records) - 1; i >= 0 && len(rows) <= limit; i-- {
		r := g.store.Records[i]
		if r.UID != uid || (before > 0 && r.ID >= before) {
			continue
		}
		rows = append(rows, r)
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	next := int64(0)
	if more && len(rows) > 0 {
		next = rows[len(rows)-1].ID
	}
	return rows, more, next
}

func (g *growthCenter) job(uid string) growthJob {
	g.mu.Lock()
	defer g.mu.Unlock()
	if j := g.jobs[uid]; j != nil {
		return *j
	}
	return growthJob{}
}

func (g *growthCenter) setJob(uid string, j growthJob) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.jobs[uid] = &j
	// 完成后 5 分钟回收，避免常驻进程里 jobs 无界增长。
	for k, v := range g.jobs {
		if !v.Running && v.FinishedAt > 0 && time.Now().Unix()-v.FinishedAt > 300 {
			delete(g.jobs, k)
		}
	}
}

func (g *growthCenter) state(uid string) growthAccountState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return cloneState(g.store.States[uid])
}

func (g *growthCenter) saveState(uid string, st *growthAccountState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st.Redeemed == nil {
		st.Redeemed = map[string]string{}
	}
	cp := cloneState(st)
	g.store.States[uid] = &cp
	g.saveLocked()
}

// refreshSnapshot 后台刷新一个账号的只读快照；并发两个账号上限。
func (g *growthCenter) refreshSnapshot(uid string, a *auth.Auth) {
	up := g.panel.cfg.Upstream
	if up == nil {
		return
	}
	for _, sec := range growthSections {
		res := up.GrowthRequest(a, http.MethodGet, sec.Path, nil)
		if !res.OK {
			g.observeError(uid, sec.Name, fmt.Sprintf("%s暂时不可用（HTTP %d）", sec.Label, res.Status))
			continue
		}
		g.observe(uid, sec.Name, res.Object(), true)
	}
	st := up.CheckinStatus(a)
	if !st.OK || st.Object()["active"] == nil {
		g.observeError(uid, "checkin", "签到状态暂时不可用")
	} else {
		g.observe(uid, "checkin", st.Object(), true)
	}
	g.mutate(func(s *growthStore) {
		snap := s.Snapshots[uid]
		if snap == nil {
			snap = &growthSnapshot{}
			s.Snapshots[uid] = snap
		}
		snap.CheckedAt, snap.RefreshRunning, snap.ExpiresAt = growthStamp(), false, 0
	})
}

// startRefresh 尝试启动刷新；已在跑或槽满时返回 running=true / false。
// 先占刷新槽、再在锁内检查并标记 running：两个并发请求不会同时给同一个账号
// 起两轮刷新（否则会重复打 11 个上游只读接口）。
func (g *growthCenter) startRefresh(uid string, a *auth.Auth) bool {
	select {
	case g.refresh <- struct{}{}:
	default:
		return false // 429：两个账号正在刷新
	}
	g.mu.Lock()
	if j := g.jobs[uid]; j != nil && j.Running {
		g.mu.Unlock()
		<-g.refresh
		return true
	}
	snap := g.store.Snapshots[uid]
	if snap != nil && snap.RefreshRunning && snap.ExpiresAt > time.Now().Unix() {
		g.mu.Unlock()
		<-g.refresh
		return true
	}
	g.jobs[uid] = &growthJob{Running: true}
	if snap == nil {
		snap = &growthSnapshot{}
		g.store.Snapshots[uid] = snap
	}
	snap.RefreshRunning, snap.ExpiresAt = true, time.Now().Add(growthRefreshTTL).Unix()
	g.saveLocked()
	g.mu.Unlock()
	go func() {
		defer func() { <-g.refresh }()
		errText := ""
		func() {
			defer func() {
				if v := recover(); v != nil {
					errText = "刷新失败，请稍后重试"
				}
			}()
			g.refreshSnapshot(uid, a)
		}()
		// 无论成败都清 running，避免页面永久停在「同步中」。
		g.mutate(func(s *growthStore) {
			if snap := s.Snapshots[uid]; snap != nil {
				snap.RefreshRunning, snap.ExpiresAt = false, 0
			}
		})
		g.setJob(uid, growthJob{FinishedAt: time.Now().Unix(), Error: errText})
	}()
	return true
}

// acquireLease 进程内互斥 + 落盘 10 分钟过期租约。
func (g *growthCenter) acquireLease() (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now().Unix()
	if g.store.Lease.Expires > now {
		return "", false
	}
	owner := fmt.Sprintf("%d-%d", now, rand.Int63())
	g.store.Lease.Owner, g.store.Lease.Expires = owner, now+int64(growthPlanLeaseTTL.Seconds())
	g.saveLocked()
	return owner, true
}

func (g *growthCenter) renewLease(owner string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.store.Lease.Owner != owner {
		return false
	}
	g.store.Lease.Expires = time.Now().Unix() + int64(growthPlanLeaseTTL.Seconds())
	g.saveLocked()
	return true
}

func (g *growthCenter) releaseLease(owner string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.store.Lease.Owner != owner {
		return
	}
	g.store.Lease.Owner, g.store.Lease.Expires = "", 0
	g.saveLocked()
}

// growthActionLabels 手动动作名（与前端按钮文案一致）。
var growthActionLabels = map[string]string{
	"redeem": "连登奖励兑换", "open": "开启盲盒", "gift": "领取礼物",
	"draw": "限时抽奖", "checkin": "每日打卡",
}

// growthPerform 执行一个明确入口的手动动作，写一条记录并使快照过期。
func (g *growthCenter) growthPerform(uid string, a *auth.Auth, action, tier string) (growthResult, error) {
	up := g.panel.cfg.Upstream
	label := growthActionLabels[action]
	st := g.state(uid)
	if st.Identity == "" {
		st.Identity = growthIdentity(uid)
	}
	day := growthDay(time.Now())
	var res upstream.SoftResult
	switch action {
	case "redeem":
		if st.Redeemed[day[:7]+":"+tier] != "" {
			return growthResult{}, errors.New("本月该档位已兑换")
		}
		current := up.GrowthRequest(a, http.MethodGet, "/activity/growth/streak", nil)
		if !current.OK {
			return growthResult{}, errors.New("无法核对奖励档位，请稍后重试")
		}
		rs, _ := current.Object()["redemption_status"].(map[string]any)
		if growthStr(rs["tier_"+tier+"_status"]) != "available" {
			return growthResult{}, errors.New("该档位尚未达标或本月已领取")
		}
		token := "redeem-" + growthIdentity(uid+":"+day[:7]+":"+tier)
		res = up.GrowthRequest(a, http.MethodPost, "/activity/growth/redeem",
			map[string]any{"tier": tier, "client_token": token})
		if res.OK {
			st.Redeemed[day[:7]+":"+tier] = day
		}
	case "open":
		if st.BoxAttemptDay == day {
			return growthResult{}, errors.New("今日已开启过盲盒，避免重复消耗能量")
		}
		quota := up.GrowthRequest(a, http.MethodGet, "/activity/growth/buddy/quota", nil)
		if !quota.OK || growthNum(quota.Object()["affordable"]) < 1 {
			return growthResult{}, errors.New("能量不足或盲盒额度暂时不可用")
		}
		st.BoxAttemptDay = day
		g.saveState(uid, &st)
		res = up.GrowthRequest(a, http.MethodPost, "/activity/growth/buddy/open", map[string]any{"count": 1})
		if res.OK {
			st.BoxConfirmedDay = day
			for _, b := range growthMapSlice(res.Object()["results"]) {
				if t, _ := b["template"].(map[string]any); t != nil {
					st.Redeemed["last_box"] = growthStr(t["name"])
					break
				}
			}
		}
	case "gift":
		travel := up.GrowthRequest(a, http.MethodGet, "/activity/growth/buddy/travel/status", nil)
		if !travel.OK || growthStr(travel.Object()["state"]) != "arrived" {
			return growthResult{}, errors.New("宠物尚未到站或礼物已领取")
		}
		res = up.GrowthRequest(a, http.MethodPost, "/activity/growth/buddy/travel/claim", map[string]any{})
	case "draw":
		lottery := up.GrowthRequest(a, http.MethodGet, "/activity/growth/lottery/summary", nil)
		if !lottery.OK || !growthBool(growthFields(lottery.Object()["module"], "enabled")["enabled"]) {
			return growthResult{}, errors.New("限时抽奖暂未开启")
		}
		chance := up.GrowthRequest(a, http.MethodGet, "/activity/growth/lottery/chances", nil)
		if !chance.OK || growthNum(chance.Object()["balance"]) < 1 {
			if st.DrawToken == "" {
				return growthResult{}, errors.New("没有可用免费抽奖次数")
			}
		}
		if st.DrawToken == "" {
			st.DrawToken = fmt.Sprintf("draw-%d-%d", time.Now().UnixNano(), rand.Int63())
		}
		g.saveState(uid, &st)
		res = up.GrowthRequest(a, http.MethodPost, "/activity/growth/lottery/draw",
			map[string]any{"client_token": st.DrawToken})
		if res.OK {
			st.DrawToken = ""
			if st.DrawDay != day {
				st.DrawDay, st.DrawCount = day, 0
			}
			st.DrawCount++
		}
	case "checkin":
		status := up.CheckinStatus(a)
		obj := status.Object()
		if growthBool(obj["today_checked_in"]) {
			return growthResult{}, errors.New("今日已经打卡领取积分")
		}
		if !growthBool(obj["active"]) {
			return growthResult{}, errors.New("签到活动未开启或状态暂时不可用")
		}
		res = up.ClaimDailyCheckinSoft(a)
		if res.OK {
			st.CheckinDay = day
		}
	default:
		return growthResult{}, errors.New("未知成长操作")
	}
	data := res.Object()
	credit := int64(0)
	for _, key := range []string{"credit_granted", "reward_credit", "credit"} {
		if res.OK && growthNum(data[key]) > credit {
			credit = growthNum(data[key])
		}
	}
	message := "已完成"
	if !res.OK {
		message = "操作未确认，请刷新状态后核对"
	}
	if action == "open" && res.OK {
		names := []string{}
		for _, b := range growthMapSlice(data["results"]) {
			if t, _ := b["template"].(map[string]any); t != nil && growthStr(t["name"]) != "" {
				names = append(names, growthStr(t["name"]))
			}
		}
		if len(names) > 0 {
			message += "：" + strings.Join(names, "、")
		}
	}
	if action == "draw" && res.OK {
		prize, _ := data["prize"].(map[string]any)
		name := growthStr(data["prize_name"])
		if name == "" && prize != nil {
			name = growthStr(prize["prize_name"])
		}
		if name == "" {
			name = "请查看官方抽奖记录"
		}
		message += "：" + name
	}
	if growthNum(data["cards_overflow"]) > 0 {
		message += fmt.Sprintf("，补登卡溢出 %d 张", growthNum(data["cards_overflow"]))
	}
	out := growthResult{
		OK: res.OK, Credit: credit, Reason: message,
		Steps: []growthStep{{Step: label, OK: res.OK, Message: message, Reward: credit}},
	}
	g.saveState(uid, &st)
	g.record(uid, "manual", label, out)
	if credit > 0 {
		g.syncBalance(uid, a)
	}
	g.invalidate(uid)
	return out, nil
}

// invalidate 领奖/开盒后让旧快照过期（前端会自动重新刷新）。
func (g *growthCenter) invalidate(uid string) {
	g.mutate(func(s *growthStore) {
		if snap := s.Snapshots[uid]; snap != nil {
			snap.CheckedAt = ""
		}
	})
}

// syncBalance 奖励入账后刷新池内余额（失败只记日志，不影响领取结果）。
func (g *growthCenter) syncBalance(uid string, a *auth.Auth) {
	p := g.panel
	if p == nil || p.cfg.Pool == nil || p.cfg.Upstream == nil {
		return
	}
	remain, total, expiring, diag, err := p.cfg.Upstream.UserResourceDetailedDiag(a, p.expiringSoonWindow())
	if err != nil {
		log.Printf("growth-plan: 余额刷新失败 uid=%s: %v", uid, err)
		return
	}
	p.cfg.Pool.SetCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsExpiringAt(uid, expiring, diag.ExpiringEnd)
}
