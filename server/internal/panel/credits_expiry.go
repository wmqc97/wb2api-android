// credits_expiry.go 面板「积分到期」视图的数据端点。
//
// 为什么单独一个端点而不是复用 /panel/api/packages：
//   - packages 只回答「这个号由哪些包构成」，排序按余额降序；
//   - 本端点回答「哪些积分**快要作废**」：按到期时间升序、按紧迫度分档，
//     并给出账号级汇总（窗口内到期的剩余合计、最早到期时刻）。
//
// 两者消费同一份上游明细（upstream.CreditPackages），但用途不同，混在一起会让
// 两个视图互相牵制（改一个的排序就要动另一个）。
//
// 口径与参考实现 admin/credits.py 一致：
//   - 只有**还有剩余额度**的包参与「最早到期」统计 —— 用完的包即使明天到期也
//     没有可抢救的额度，算进来只会让调度白跑一趟；
//   - 已过期但账面仍有剩余的额度单独列出（上游扣减延迟/周期错位），不计入可抢救额度；
//   - 紧迫度分档（越小越先被调度），与 pool.expiryRank 同一口径：
//     0 = 窗口内到期且还有剩余（不烧就作废）
//     1 = 有明确到期时间但还早
//     2 = 长期有效（无作废风险，只用它兜底）
package panel

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// creditDefaultWindowDays 上游没配 expiring_soon 时的缺省窗口（天）。
// 官方客户端按 7 天把额度标成橙色，调度也按这个窗口抢先消耗。
const creditDefaultWindowDays = 7

// parsePackageEnd 解析上游包的到期墙钟串（"2006-01-02 15:04:05"，UTC+8）。
// 空串或不可解析 → 零值（= 上游没给到期时间，前端展示「长期有效」，绝不臆造日期）。
func parsePackageEnd(s string) time.Time {
	if len(s) < 10 {
		return time.Time{}
	}
	cst := time.FixedZone("CST", 8*3600)
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, cst); err == nil {
		return t
	}
	// 只有日期：按当日 23:59:59 到期（当天仍可用）。
	if t, err := time.ParseInLocation("2006-01-02", s[:10], cst); err == nil {
		return t.Add(24*time.Hour - time.Second)
	}
	return time.Time{}
}

// creditPkgRow 一个积分包的到期视图。
type creditPkgRow struct {
	UID          string   `json:"uid"`
	Nickname     string   `json:"nickname,omitempty"`
	Realm        string   `json:"realm,omitempty"`
	Name         string   `json:"name"`
	PackageCode  string   `json:"package_code,omitempty"`
	Size         int64    `json:"size"`
	Remain       int64    `json:"remain"`
	ExpireAt     *int64   `json:"expire_at"` // Unix 毫秒；null = 长期有效
	DaysLeft     *float64 `json:"days_left"`
	ExpiringSoon bool     `json:"expiring_soon"`
	Expired      bool     `json:"expired"`
}

// creditAccRow 一个账号的到期汇总。
type creditAccRow struct {
	UID            string   `json:"uid"`
	Nickname       string   `json:"nickname,omitempty"`
	Realm          string   `json:"realm,omitempty"`
	Credits        int64    `json:"credits"`
	CreditsTotal   int64    `json:"credits_total"`
	ExpiringSoon   int64    `json:"expiring_soon"`
	ExpiredRemain  int64    `json:"expired_remaining"`
	Soonest        *int64   `json:"soonest_expire_at"`
	DaysLeft       *float64 `json:"days_left"`
	Rank           int      `json:"rank"`
	PackageCount   int      `json:"package_count"`
	ActivePackages int      `json:"active_package_count"`
	Error          string   `json:"error,omitempty"`
}

// urgencyRank 账号级紧迫度分档（与 pool 的 expiryRank 同一口径）：
// 有窗口内到期剩余 → 0；有到期时间 → 1；否则 2。
func urgencyRank(expiringSoon int64, soonest *int64) int {
	if expiringSoon > 0 {
		return 0
	}
	if soonest != nil {
		return 1
	}
	return 2
}

// creditsExpiry 汇总所有账号的逐包到期信息，按「最紧迫优先」返回。
//
//	GET /panel/api/credits/expiry
//
// 每个账号一次上游探询（与 packages 端点同源、同并发度 3）；单个账号失败只在该行
// 标 error，不影响其它账号——一份坏数据不该让整页空掉。
func (p *Panel) creditsExpiry(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	windowDays := creditDefaultWindowDays
	if p.cfg.Scheduler != nil {
		if d := p.cfg.Scheduler.ExpiringSoonWindow(); d > 0 {
			if n := int(d / (24 * time.Hour)); n > 0 {
				windowDays = n
			} else {
				windowDays = 1
			}
		}
	}
	now := time.Now()
	windowEnd := now.Add(time.Duration(windowDays) * 24 * time.Hour)

	accs := make([]creditAccRow, len(accts))
	pkgs := make([]creditPkgRow, 0, len(accts)*4)

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, s := range accts {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			row := creditAccRow{
				UID: s.UID, Nickname: s.Nickname, Realm: s.Realm,
				Credits: s.Credits, CreditsTotal: s.CreditsTotal, Rank: 2,
			}
			a := p.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				row.Error = "account not loaded"
				accs[i] = row
				return
			}
			if p.cfg.Upstream == nil {
				row.Error = "upstream 未配置"
				accs[i] = row
				return
			}
			packs, _, _, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				row.Error = err.Error()
				accs[i] = row
				return
			}
			var expiring, expiredRemain int64
			var soonest *int64
			local := make([]creditPkgRow, 0, len(packs))
			for _, pk := range packs {
				item := creditPkgRow{
					UID: s.UID, Nickname: s.Nickname, Realm: s.Realm,
					Name: pk.Name, PackageCode: pk.PackageCode,
					Size: pk.Size, Remain: pk.Remain,
				}
				if t := parsePackageEnd(pk.EndTime); !t.IsZero() {
					ms := t.UnixMilli()
					item.ExpireAt = &ms
					days := t.Sub(now).Hours() / 24
					if days < 0 {
						days = 0
					}
					item.DaysLeft = &days
					item.Expired = !t.After(now)
					item.ExpiringSoon = !item.Expired && t.Before(windowEnd)
				}
				// 汇总只看「还有剩余」的包（用完的包没有可抢救额度）。
				if pk.Remain > 0 {
					row.ActivePackages++
					if item.Expired {
						expiredRemain += pk.Remain
					}
					if item.ExpiringSoon {
						expiring += pk.Remain
					}
					if item.ExpireAt != nil && (soonest == nil || *item.ExpireAt < *soonest) {
						soonest = item.ExpireAt
					}
				}
				local = append(local, item)
			}
			row.ExpiringSoon = expiring
			row.ExpiredRemain = expiredRemain
			row.Soonest = soonest
			row.PackageCount = len(packs)
			if soonest != nil {
				days := float64(*soonest-now.UnixMilli()) / 86400000
				if days < 0 {
					days = 0
				}
				row.DaysLeft = &days
			}
			row.Rank = urgencyRank(expiring, soonest)
			accs[i] = row

			mu.Lock()
			pkgs = append(pkgs, local...)
			mu.Unlock()
		}(i, s)
	}
	wg.Wait()

	// 账号排行：紧迫度 → 最早到期 → 快过期额度多的靠前（同级内）。
	sort.SliceStable(accs, func(i, j int) bool {
		a, b := accs[i], accs[j]
		if a.Rank != b.Rank {
			return a.Rank < b.Rank
		}
		ai, bi := int64(1<<62), int64(1<<62)
		if a.Soonest != nil {
			ai = *a.Soonest
		}
		if b.Soonest != nil {
			bi = *b.Soonest
		}
		if ai != bi {
			return ai < bi
		}
		return a.ExpiringSoon > b.ExpiringSoon
	})

	// 包列表：只留下还有剩余的包，按到期时间升序（无到期的排最后）。
	sort.SliceStable(pkgs, func(i, j int) bool {
		a, b := pkgs[i], pkgs[j]
		if (a.ExpireAt == nil) != (b.ExpireAt == nil) {
			return a.ExpireAt != nil // 有明确到期时间的排前面
		}
		if a.ExpireAt != nil && *a.ExpireAt != *b.ExpireAt {
			return *a.ExpireAt < *b.ExpireAt
		}
		return a.Remain > b.Remain
	})
	active := make([]creditPkgRow, 0, len(pkgs))
	for _, it := range pkgs {
		if it.Remain > 0 {
			active = append(active, it)
		}
	}

	var totalExpiring, totalExpired int64
	var expiringAccounts int
	var soonestAll *int64
	for _, a := range accs {
		totalExpiring += a.ExpiringSoon
		totalExpired += a.ExpiredRemain
		if a.Rank == 0 {
			expiringAccounts++
		}
		if a.Soonest != nil && (soonestAll == nil || *a.Soonest < *soonestAll) {
			soonestAll = a.Soonest
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"window_days": windowDays,
		"summary": map[string]any{
			"accounts":                len(accs),
			"expiring_accounts":       expiringAccounts,
			"expiring_soon_remaining": totalExpiring,
			"expired_remaining":       totalExpired,
			"soonest_expire_at":       soonestAll,
			"packages":                len(active),
		},
		"accounts": accs,
		"packages": active,
	})
}

// creditExpiryBadge 只算「有几个账号的积分会在窗口内作废」，供侧栏角标使用。
// 便宜（走池内已同步的快过架子集，不打上游）——侧栏是每次进页面都要渲染的，
// 绝不能让它触发 N 次上游探询。
//
//	GET /panel/api/credits/badge
func (p *Panel) creditExpiryBadge(w http.ResponseWriter, r *http.Request) {
	var n int
	var soonest *int64
	for _, s := range p.cfg.Pool.List() {
		if s.CreditsExpiring > 0 && !s.Disabled && !s.ManualDisabled {
			n++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expiring_accounts": n, "soonest_expire_at": soonest})
}
