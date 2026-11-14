// 选号：Pick 簇（healthy 三因子加权 Top5 短名单 + 加权随机 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"fmt"
	"log"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Pick 单一选号入口（无请求级轮换、无 realm 过滤，模型感知缺省账号级）。
// 需要请求级轮换（tried）或分池（realm）时用 PickExcludingForRealm。
func (p *Pool) Pick() *auth.Auth {
	return p.pick(nil, "", "", false)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried, "", "", false)
}

// PickExcludingForModel 模型感知选号：等同 PickExcluding，但对「6004 模型级冷却中的
// 账号」进行模型豁免——请求模型与其 trigger 模型不同时视为可用（issue #31）。
// reqModel 为空时即普通 PickExcluding（不影响既有调用语义）。
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pick(tried, reqModel, "", false)
}

// PickExcludingForRealm 模型感知 + 分池选号：候选集先按 Realm()==realm 过滤
// （realm 空 = 不过滤，退化为 PickExcludingForModel），再按模型健康口径判定。
// 供 handler 在 global/cn 双域下分流（global 模型请求只路由 global 账号）。
//
// **硬过滤**（realm 谓词不可绕过）：本域无候选直接返回 nil，不跨域。目录拉取/面板
// 按域查询必须用本入口——global 账号打 CN 的 /console 家族会吃 500 网关错误页
// （「拉取模型 500」的历史根因），跨域回落会重新引入该类错配。chat 裸名路由用
// PickExcludingForRealmFallback（软优先）。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm, false)
}

// PickExcludingForRealmFallback 模型感知 + realm **软优先**选号：先按 Realm()==realm
// 过滤选，本域选不出账号时才去掉 realm 谓词、回落另一域再选一次——单次调用内回落
// 至多一次，不在两域间反复横跳（不破坏 realm_precedence 语义与多域隔离）。
//
// "本域选不出"的判定顺序（详见 pick 的四段说明）：本域 healthy 候选为空即回落
// 另一域 healthy 候选；本域只剩冷却号而另一域有健康号时也回落健号（探测冷却号是
// 最后手段）；仅当两域都无 healthy 候选才走全冷却兜底，且兜底同样先本域、后另一域。
//
// 回落轮只放宽 realm 谓词，其余谓词（tried / healthy / healthyForModel / inFlightFull
// / 保留积分闸门）逐一保持：更宽松的域不改变请求级轮换与租约语义，只是候选域从
// 「本域」扩到「全池」。每轮回落仍受调用方 tried 约束，故请求整体尝试次数上限不变
// （MaxRotate 次）。
//
// 动机（issue #199c）：混合池（1 global + 1 cn、realm_precedence=global）下，裸模型名
// 归属 global；若 global 号对该模型全部限流/不可用，旧实现轮转的每一轮都只在 global
// 域里找候选 → 无候选 503，而 cn 号完全可用却从未被尝试（显式 cn: 前缀同一请求 200）。
// handler 仅对**裸名归属**启用回落（显式 "cn:"/"global:" 前缀是用户强指定，不回落）。
func (p *Pool) PickExcludingForRealmFallback(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm, true)
}

// PickExcludingForRealmMeta 元数据路径选号（拉模型目录 / 面板查询这类 GET）：
// 等同 PickExcludingForRealm(nil, "", realm)，但**豁免保留积分闸门**。
//
// 为什么需要独立入口：这些调用方传 reqModel=""（只要"任一个本域账号"去取元数据），
// 而空模型名在保留积分口径下是**保守拦截**的（见 reserve.go 文件头）。若不豁免，
// 池内账号全部触底时「模型与档位」会 503、/v1/models 退回静态名单、/v1/stats 的倍率
// 快照失效——恰好发生在用户最需要看清"还剩什么免费模型"的时刻。这类请求不消费积分，
// 闸门拦它没有收益只有代价。
//
// 注意：**只有元数据路径**用它。任何会真正发起 chat / 签到 / 任务出站的调用方都不该
// 走这里——那正是闸门要拦的消费路径。
func (p *Pool) PickExcludingForRealmMeta(tried map[string]bool, realm string) *auth.Auth {
	return p.pickWith(tried, "", realm, false, true)
}

// PickForCatalog 元数据（模型目录 / 价格展示）专用选号：**允许**选中正在硬冷却
// （余额耗尽）的账号。
//
// 为什么必须开这个口子：模型目录是只读接口，不消费任何积分，余额为 0 的号照样能
// 返回目录。若沿用 chat 的选号谓词（CoolHard 一律排除），一旦某个域的账号全部烧光
// 余额（典型：纯 global 池的两个号 credits=0 处于硬冷却），面板「模型与档位」就会
// 报「没有可用的 global 账号」——恰恰是用户最需要看清「还剩什么模型、多少钱」的时刻。
//
// 优先级（同级内按余额降序、UID 升序，保证结果稳定可解释）：
//  1. 该域的 healthy 账号（正常情况下第 1 段就命中，与既有行为一致）；
//  2. 该域内处于冷却期（软冷却/熔断/硬冷却）的账号——只要不是被禁用/临时停用。
//
// 绝不返回 disabled / manualDisabled（那是运维明确的处置，不该被展示路径绕过）。
// 只按 realm 精确过滤：跨域借号会拉到另一个域的目录（modelsPaths 按账号 realm 选路），
// 展示结果会串域，比报错更糟。
func (p *Pool) PickForCatalog(realm string) *auth.Auth {
	return p.pickCatalog(realm, false)
}

// PickForCatalogAny 同 PickForCatalog，但连**禁用/临时停用**的账号也借。
//
// 何时该用它：只读的目录探测要多拿一份数据（倍率/窗口/能力）时——账号被停用通常
// 是"流量不想要它了"，不代表它的 token 不能读目录。拉目录失败没有任何副作用
// （不扣积分、不改状态），所以这里放宽到"池里还有谁就试谁"，拿不到再回落静态表。
//
// 只用于元数据探测；聊天/签到等一切会花积分或写状态的路径都不许用。
func (p *Pool) PickForCatalogAny(realm string) *auth.Auth {
	return p.pickCatalog(realm, true)
}

func (p *Pool) pickCatalog(realm string, includeDisabled bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var best *entry
	bestRank := 99
	for uid, e := range p.byUID {
		if !includeDisabled && (e.disabled || e.manualDisabled) {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		rank := 1 // 冷却中（含余额耗尽的硬冷却）
		if e.healthy(now) {
			rank = 0
		}
		if rank > bestRank {
			continue
		}
		if rank == bestRank && best != nil {
			if e.credits < best.credits {
				continue
			}
			if e.credits == best.credits && uid > best.a.UID {
				continue
			}
		}
		best, bestRank = e, rank
	}
	if best == nil {
		return nil
	}
	log.Printf("pool: catalog_pick uid=%s realm=%s rank=%d credits=%d（元数据查询：允许用冷却号）",
		best.a.UID, realm, bestRank, best.credits)
	return best.a
}

// pick 在 healthy 候选集中按权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效）。
// realm 非空时候选过滤叠加 Realm()==realm 谓词（分池选号域）；realmFallback=true 时
// 本域选不出任何账号才去掉该谓词回落另一域重选一次（跨域回落，成功时打一条
// "pool: realm fallback" 日志供运维确认跨域发生）。
//
// 四段顺序（前一段有果即返回，不跨段跳级；跨域只放宽 realm 谓词，其余谓词逐一保持）：
//  1. 本域 healthy 候选（含 healthyForModel + 保留积分闸门）；
//  2. [回落] 另一域 healthy 候选——必须排在全冷却兜底**之前**：本域只剩冷却号而
//     另一域有健康号时，「优先本域」的合理边界止于 healthy，探测冷却号是最后手段；
//  3. 本域全冷却兜底（既有语义：全池冷却时探测最早到期号而非 503）；
//  4. [回落] 另一域全冷却兜底——本域连冷却号都没有时的最后手段（仍优于 503）。
//
// 保留积分闸门（pool.reserve_credits）贯穿 1–4 段（metaOnly=false 时）：它回答的是
// "这个号对这个模型此刻能不能用"，与 healthyForModel 同一层语义，故每一段都不得绕开
// ——尤其第 3/4 段的全冷却兜底：兜底是"探测冷却号是否已恢复"的最后手段，不是"花掉
// 用户设的积分底线"的旁路（余额触底的号在贵模型上被兜底选中，用户设的底线就形同虚设）。
func (p *Pool) pick(tried map[string]bool, reqModel, realm string, realmFallback bool) *auth.Auth {
	return p.pickWith(tried, reqModel, realm, realmFallback, false)
}

// pickWith 是 pick 的实现体；metaOnly=true 时跳过保留积分闸门（见
// PickExcludingForRealmMeta 的注释：只有元数据路径该传 true）。
func (p *Pool) pickWith(tried map[string]bool, reqModel, realm string, realmFallback, metaOnly bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// 闸门在入口按 reqModel 现算一次，四段共用（理由见 reserveGate 的类型注释）。
	gate := p.reserveGateFor(reqModel, metaOnly)
	if a := p.pickHealthyLocked(tried, now, reqModel, realm, gate); a != nil {
		return a
	}
	if realm != "" && realmFallback {
		if a := p.pickHealthyLocked(tried, now, reqModel, "", gate); a != nil {
			return p.logRealmFallbackLocked(realm, a, reqModel)
		}
	}
	if a := p.pickEarliestExpiryLocked(tried, now, reqModel, realm, gate); a != nil {
		return a
	}
	if realm != "" && realmFallback {
		if a := p.pickEarliestExpiryLocked(tried, now, reqModel, "", gate); a != nil {
			return p.logRealmFallbackLocked(realm, a, reqModel)
		}
	}
	return nil
}

// logRealmFallbackLocked 打一条跨域回落日志并返回该账号（运维据此确认跨域发生）。
// 调用方必须已持 p.mu 写锁（在临界区内取 a.Realm()，与选号口径同一时刻快照）。
func (p *Pool) logRealmFallbackLocked(realm string, a *auth.Auth, reqModel string) *auth.Auth {
	log.Printf("pool: realm fallback %s -> %s (model=%s)", realm, a.Realm(), reqModel)
	return a
}

// PickMode 选号模式（config pool.pick_mode 的运行期镜像）。
//
// 两种模式的分野是「候选集怎么挑」这一件事，其余谓词（tried / healthy /
// healthyForModel / inFlightFull / realm 过滤 / 全冷却兜底 / 跨域回落）逐一相同——
// 模式只换 healthy 段的挑选方式，不换任何过滤条件与兜底语义。
type PickMode int

const (
	// PickWeighted 三因子加权随机（**缺省**，零值即本模式）：改动前的既有行为，
	// 逐字节不变。Top5 短名单 + 加权抽签 + minPickGap 防并发撞号 + LRU 兜底。
	PickWeighted PickMode = iota
	// PickSequential 顺序填充式：按运维顺序 + 快过期账号前置排序取**第一个**合格
	// 账号，靠 inFlightFull 过滤实现「并发满了临时溢出给下一个号、并发降回来它重新
	// 成为首选」的语义（顺序遍历是确定性的，不需要任何额外逻辑）。
	PickSequential
)

// SetPickMode 注入选号模式（main 从 config pool.pick_mode 解析后调用；
// 面板改配置后热生效，下一次选号即按新模式）。未知值由调用方（config）归一化，
// 本入口只做「枚举外的值一律当缺省 weighted」的兜底。
func (p *Pool) SetPickMode(m PickMode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch m {
	case PickSequential:
		p.pickMode = PickSequential
	default:
		p.pickMode = PickWeighted
	}
}

// PickMode 返回当前生效的选号模式（供面板/运维接口回显）。
func (p *Pool) PickMode() PickMode {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pickMode
}

// pickHealthyLocked 在 healthy 候选集中选号（realm 谓词为硬过滤）；无候选返回 nil，
// 交由 pick 决定是否跨域回落或走全冷却兜底。调用方必须已持 p.mu 写锁：跨域回落要在
// 同一临界区内做多次候选扫描，中间不得释放锁——否则两次扫描之间池状态变化会让
// 「回落」判定与候选集不一致。
// gate 为保留积分闸门（由 pick 按 reqModel 现算一次后传入，理由见 reserveGate 注释）。
func (p *Pool) pickHealthyLocked(tried map[string]bool, now time.Time, reqModel, realm string, gate reserveGate) *auth.Auth {
	if p.pickMode == PickSequential {
		return p.pickSequentialLocked(tried, now, reqModel, realm, gate)
	}
	return p.pickWeightedHealthyLocked(tried, now, reqModel, realm, gate)
}

// seqSkip 顺序模式下被跳过的账号及其原因（仅供溢出日志组装，不参与选号判定）。
type seqSkip struct {
	uid    string
	reason string
}

// seqSkipReserve 顺序模式下"因保留积分跳过"的原因标签。
//
// 为什么要单列一类（而不是并进 unhealthy）：它与 tried/inflight 一样是**持久条件**——
// 账号一旦触底，此后每一次贵模型选号都会跳过它。若按 overflow 处理，
// logSequentialSkipsLocked 会把每一行都变成 `uid=poor reason=reserve`，把"顺序填充的
// 溢出链路"这个真正需要人看的信号淹掉。故它**不算** overflow：只有它（与 realm）时不打
// 汇总行，那条事实由 reserveLogThrottle 的节流日志承担（每个 uid 每分钟一条），
// 既可见又不刷屏；与真正的溢出（tried/unhealthy/inflight_full）同时出现时，汇总行照打
// 并把 reserve 项一并列出（用户能看到完整的跳过链路）。
const seqSkipReserve = "reserve"

// pickSequentialLocked 顺序填充式选号：按 sequentialPickOrderLocked()（运维顺序 +
// 快过期账号前置）从上到下遍历，返回第一个同时满足 tried 未标记 /
// healthy(ForModel) / 保留积分闸门 / 未占满在途 的账号。
//
// 三条需求的实现方式（都不需要额外状态机）：
//   - 需求 2「并发满 = 临时溢出」：inFlightFull 过滤天然实现——顺序靠前的号满了就
//     落到下一个；并发降回来后它重新是第一个合格候选（顺序遍历确定性保证）。
//   - 需求 3「用完（带恢复时间的 429）= 真正切换」：applyErrorPolicy 的
//     CooldownSoftRate/CooldownSoftForModel 已把冷却写进 until/modelCooldowns，
//     healthy/healthyForModel 为 false，顺序遍历自然跳过；到期后重新首选。
//   - 需求 4「临时 429 重试一次后仍 429 立即切下一个」：handler 的 tried 集合 +
//     本函数跳过 tried —— 轮转天然落到下一个号（配合需求 5 的不退避，切换零延迟）。
//
// 为什么 sequential 分支**跳过 minPickGap 过滤**（minPickGap 只服务加权模式）：
// 那个 100ms 窗口的语义是「同一账号在极短窗口内不重复被选中」，它针对的是加权模式的
// **随机抽签**——随机源可能在连续多次选号里反复抽中同一个号，靠时间窗把它挤开。
// 顺序模式下选号是**确定性**的：同一个号被连续选中不是缺陷，而正是需求本身
// （"一个号一个号地用，把这个号的额度用完再下一个"）。若在顺序模式保留该过滤，
// 同一个号在前一次选中的 100ms 内会因 `now.Sub(e.lastUsed) < minPickGap` 被跳过，
// 落到**下一个号**——制造出「明明没满却跳到下一个号」的假溢出，正好破坏顺序语义
// （并发未满时请求被分散到后面的号，用户看到的仍是"每次挑不同账号"）。
// 另注：minPickGap 的实现把 lastUsed 在锁内即时置为 now，且候选全被窗口挡住时走
// LRU 兜底（可能选中窗口外的其他号）——这两条在顺序模式下都是反向语义。
// 故本分支不做该过滤，且不读也不写 lastUsed 之外的任何窗口判定。
//
// 保留积分闸门（gate）与 healthyForModel 同层：余额触底的号对贵模型视同不可用，
// 顺序遍历自然跳过它、落到下一个号；免费模型则照常命中顺序第一的号。
//
// 溢出日志（需求 6）：只有当**顺序靠前的合格形态账号被跳过**时才打一条，
// 避免每次选号都刷屏。触发情形三类：在途占满（inflight_full）、冷却/禁用等不健康
// （unhealthy）、保留积分触底（reserve）。跳过原因取每个被跳过账号的首个判据
// （顺序：tried → realm → healthy → reserve → inflight）。
func (p *Pool) pickSequentialLocked(tried map[string]bool, now time.Time, reqModel, realm string, gate reserveGate) *auth.Auth {
	order := p.sequentialPickOrderLocked()
	var chosen *entry
	// skipped 收集被跳过的账号及其原因（仅在真的选中了后面的号时才打日志：
	// 无候选返回 nil 时由 pick 决定跨域回落/兜底，那些路径有自己的日志）。
	var skipped []seqSkip
	for _, uid := range order {
		e := p.byUID[uid]
		if tried != nil && tried[uid] {
			// 请求级轮换：本请求已经试过该号（含临时 429 重试一次后的第二次尝试），
			// 直接跳过——需求 4 的「还是 429 就立即切下一个」靠这里落地。
			skipped = append(skipped, seqSkip{uid, "tried"})
			continue
		}
		e.pruneExpiredModelCooldowns(now) // 惰性清理过期模型级冷却（防 map 膨胀）
		if realm != "" && e.a.Realm() != realm {
			skipped = append(skipped, seqSkip{uid, "realm"})
			continue
		}
		healthy := e.healthy(now)
		if reqModel != "" {
			healthy = e.healthyForModel(now, reqModel)
		}
		if !healthy {
			// 需求 3：带恢复时间的 429 已在 applyErrorPolicy 写成 until/模型级冷却，
			// 此处自然跳过（reason 文案区分账号级与模型级，便于用户验证）。
			skipped = append(skipped, seqSkip{uid, "unhealthy"})
			continue
		}
		if !gate.allows(e, reqModel) {
			// 保留积分：余额 ≤ 保留线且该模型非免费 → 视同对该模型不可用，跳过。
			skipped = append(skipped, seqSkip{uid, seqSkipReserve})
			continue
		}
		if p.inFlightFull(e) {
			// 需求 2：并发满 → 临时溢出给下一个号（不冷却、不惩罚，并发降回来即首选）。
			skipped = append(skipped, seqSkip{uid, "inflight_full"})
			continue
		}
		chosen = e
		break
	}
	if chosen == nil {
		return nil // 无合格候选：交给 pick 跨域回落 / 全冷却兜底
	}
	p.logSequentialSkipsLocked(skipped, chosen)
	chosen.lastUsed = now // 与加权路径同口径：供面板/观测看到"最近被选中"
	p.pickSeq++
	chosen.usedSeq = p.pickSeq
	return chosen.a
}

// logSequentialSkipsLocked 顺序模式下发生溢出时打一条日志（需求 6 可观测）：
// 用户据此验证「顺序靠前的号因为满/冷却被跳过、本次落到哪个号」。
// 只在**确实跳过了账号且最终选中了更靠后的号**时打；一次选号至多一条（不刷屏），
// 且每个被跳账号的原因用 `uid=... reason=...` 逐项列出。
// 在途占满额外带 `(在途/上限)` 便于用户核对并发档位（maxInFlight 按 realm 分档）。
//
// **纯 realm / 纯 reserve 跳过不打汇总日志**：混合池下顺序里排在前面的是另一域的号时，
// 按域过滤的每次请求都会跳过它们（reason=realm）——那是分池路由的正常形态，不是溢出，
// 照打会把每次请求都刷成一行；reserve 同理（账号触底后每次贵模型请求都会跳过它），
// 那条事实由 reserveLogThrottle 的**节流**日志承担（每个 uid 每分钟至多一条）。
// 判据取「至少一个非 realm、非 reserve 的跳过原因」（tried / unhealthy / inflight_full
// 三类才是溢出/切换信号），有它时整条 skip 列表（含 realm/reserve 项）一并列出，
// 用户能看到完整的跳过链路。
// 调用方必须已持 p.mu 写锁（读 inFlight/上限字段与选号同一时刻快照）。
func (p *Pool) logSequentialSkipsLocked(skipped []seqSkip, chosen *entry) {
	overflow := false
	for _, s := range skipped {
		if s.reason != "realm" && s.reason != seqSkipReserve {
			overflow = true
			break
		}
	}
	if !overflow {
		return // 顺序第一直接命中，或仅因 realm/保留积分过滤跳过：正常路径零噪音
	}
	var b strings.Builder
	b.WriteString("pool: sequential skip")
	for _, s := range skipped {
		b.WriteString(" uid=")
		b.WriteString(s.uid)
		b.WriteString(" reason=")
		b.WriteString(s.reason)
		if s.reason == "inflight_full" {
			// 带在途/上限：maxInFlight 未设（0=不限）时 inFlightFull 恒 false，
			// 不会走到这里；limit>0 才有 inflight_full 这条原因。
			e := p.byUID[s.uid]
			if e != nil {
				fmt.Fprintf(&b, "(%d/%d)", e.inFlight.Load(), p.inFlightLimit(e))
			}
		}
	}
	b.WriteString(" -> next uid=")
	b.WriteString(chosen.a.UID)
	log.Print(b.String())
}

// pickWeightedHealthyLocked 三因子加权随机的 healthy 段选号（**改动前原实现，
// 逐字保留**）：Top5 短名单 + 加权抽签 + minPickGap 防并发撞号 + LRU 兜底。
// 由 pickHealthyLocked 在 PickWeighted 模式（缺省）下调用——除保留积分闸门（gate）
// 这一条新增过滤外，本函数体与 pick_mode 引入前逐字节相同，保证默认部署零回归。
func (p *Pool) pickWeightedHealthyLocked(tried map[string]bool, now time.Time, reqModel, realm string, gate reserveGate) *auth.Auth {
	realmOK := func(e *entry) bool { return realm == "" || e.a.Realm() == realm }
	healthyOf := func(e *entry) bool { return realmOK(e) && e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return realmOK(e) && e.healthyForModel(now, reqModel) }
	}
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		e.pruneExpiredModelCooldowns(now) // 惰性清理过期模型级冷却（防 map 膨胀）
		if !healthyOf(e) {
			continue
		}
		if !gate.allows(e, reqModel) {
			continue // 保留积分：余额 ≤ 保留线且该模型非免费 → 视同对该模型不可用
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 本段无 healthy 候选：返回 nil 由 pick 决定跨域回落或全冷却兜底
		// （不得在此处直接兜底，否则跨域回落会被"本域冷却号"抢先，本域全冷却而
		// 另一域健康时仍打在注定失败的冷却号上）。
		return nil
	}
	// 到期紧迫度：所有策略的**首要**排序依据（参考实现 pool.expiry_key）。
	//
	// 为什么是"先分组再加权"而不是"再加一个权重项"：权重是概率倾斜 —— 一个 7 天内
	// 到期的号只是「更容易」被选中；只要池里还有余额更高或更闲置的号，抽签就可能
	// 一直不选它，直到那批额度作废（用户原话：「还是优先把快到期的账号积分先用完」）。
	// 因此这里只保留**最紧迫那一档**的候选，组内仍走原三因子加权 Top5 + 抽签 ——
	// 「严格优先」与「打散热点」两者同时成立。
	//
	// 零回归：没有任何到期信息时全体 rank=2，过滤是恒等操作，行为与改动前逐字节一致。
	best := 2
	for _, e := range cands {
		if r := e.expiryRank(); r < best {
			best = r
		}
	}
	if best < 2 {
		kept := cands[:0]
		for _, e := range cands {
			if e.expiryRank() == best {
				kept = append(kept, e)
			}
		}
		cands = kept
	}
	// top5 短名单按三因子权重降序截断（而非 credits 单纯降序）：否则闲置补偿 + 成功率
	// 根本进不了短名单决策，低 credits 但高成功率/久置的账号会永远排不进 top5。
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	// 权重只算一次：顶 5 截断要排序，若在 sort 比较器里现算 weightOf 会翻成 O(n log n) 次
	// 冗余浮点计算（46 账号约 500 次）。先做 O(n) 预计算，再按 (权重, uid) 排序。
	type weighted struct {
		e *entry
		w float64
	}
	ws := make([]weighted, len(cands))
	for i, e := range cands {
		ws[i] = weighted{e: e, w: p.weightOf(e, maxCredits, now)}
	}
	// 等权重洗牌：仅当存在权重相等且候选数超过 top5 时，才对 ws 做 Fisher-Yates
	// 洗牌（且**不消耗 p.randInt64N 注入源**，避免改变 pickWeighted 的确定性语义，
	// 见 TestPickDeterministicViaSetRandomSource）。权重全等或存在并列时，按字典序
	// 截断会让 uid 靠后的账号永远进不了 top5（等权重账号被字典序饿死、LRU 兜底
	// 又只在 top5 内转——惊群集中单号的根因）。洗牌用独立的 time-seeded 源，
	// 只在截断边界制造等权重随机次序，不影响加权抽签本身的确定性。
	if len(ws) > 5 {
		eq := false
		for i := 1; i < len(ws); i++ {
			if ws[i].w == ws[0].w {
				eq = true
				break
			}
		}
		if eq {
			shuf := rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(len(ws))))
			shuf.Shuffle(len(ws), func(i, j int) { ws[i], ws[j] = ws[j], ws[i] })
		}
	}
	sort.SliceStable(ws, func(i, j int) bool {
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID // 稳定兜底（洗牌后此项几乎不触发）
	})
	cands = cands[:0]
	for _, c := range ws {
		cands = append(cands, c.e)
	}
	// candsAll 保留截断前的全候选（权重降序），供 LRU 兜底在全量范围选最旧者，
	// 避免 top5 字典序截断把等权重靠后账号饿死（惊群根因之一）。
	candsAll := cands
	if len(cands) > 5 {
		cands = cands[:5]
	}
	// 防并发撞号：在持锁内基于「上次选中时刻」过滤，但同一批并发 goroutine 会串行进入
	// 本函数（写锁），每个进入者都把 lastUsed 置为 now —— 于是同一瞬间的第 2..N 个
	// 进入者看到前一个账号 lastUsed==now（距今 0 < minPickGap），被自然挤向其他账号。
	// 关键：lastUsed 在锁内赋值，使时间窗口判定在并发下可重入。
	eligible := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if now.Sub(e.lastUsed) >= minPickGap {
			eligible = append(eligible, e)
		}
	}
	var e *entry
	if len(eligible) == 0 {
		// top5 全部刚被用过：LRU 兜底，在**全候选 candsAll**（非仅 top5）里选最旧者。
		// 用 usedSeq 单调序号而非 lastUsed 墙钟比较：Windows 等平台 time.Now() 精度
		// ~0.5ms，快速连续选号时所有 lastUsed 完全相等，Before 全 false 会恒选
		// candsAll[0] 导致集中。usedSeq 严格全序，与时间精度无关。
		e = candsAll[0]
		for _, c := range candsAll[1:] {
			if c.usedSeq < e.usedSeq {
				e = c
			}
		}
	} else {
		e = p.pickWeighted(eligible) // eligible 保序 = top5 降序子集
	}
	e.lastUsed = now // 锁内即时标记：下一个进入 pick 的 goroutine 立即看到本号已用
	p.pickSeq++
	e.usedSeq = p.pickSeq // 单调序号：保证 usedSeq 严格全序（防惊群/LRU 的权威依据）
	return e.a
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用/非临时停用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 与 manualDisabled 永不参与（前者系统判死、后者运维摘除，都不该被兜底放行）；
// CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
//
// 保留积分闸门（gate）在本段同样生效：兜底是"探测冷却号是否已恢复"的最后手段，不是
// "花掉用户设的积分底线"的旁路——余额已触底的号在贵模型上被兜底选中，底线就形同虚设
// （用户明确要求"贵模型把积分用到剩余 50 了就不能再用这个号了"）。免费模型不受影响。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, reqModel, realm string, gate reserveGate) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // 域过滤：池内跨 realm 的冷却账号不参与本 realm 兜底
		}
		if e.disabled || e.manualDisabled || e.dailyTokenBlocked(now, reqModel) {
			continue // 禁用/临时停用的账号永不参与兜底
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
		}
		if !gate.allows(e, reqModel) {
			continue // 保留积分：兜底不得绕开（见函数注释）
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("pool: fallback_earliest_expiry uid=%s until=%s kind=%s", best.a.UID, best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	// 兜底同样是「选中」，必须与 pick() 正常路径、粘性命中路径（PickByUIDForModel）
	// 一样推进 usedSeq/pickSeq：否则被兜底反复选中的账号 usedSeq 恒为 0，在 pick 的
	// LRU 兜底（按 usedSeq 取最旧）眼里永远是「最旧」，刚被用过就被立刻再选——
	// 防集中/防惊群失效（entry.usedSeq 契约：每次被选中时取 p.pickSeq 自增值）。
	p.pickSeq++
	best.usedSeq = p.pickSeq
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（上限 0 = 不限 → 恒 false）。
// 调用方需已持 p.mu（读锁或写锁均可，本方法只读上限字段）。上限按 realm
// 分档（global 档 maxInFlightGlobal，WAF 403 修复 P1-1；未设置回落 maxInFlight）。
func (p *Pool) inFlightFull(e *entry) bool {
	limit := p.inFlightLimit(e)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// pickWeighted 三因子加权随机（claude-api selectWeightedRandom 参考口径）：
//
//		weight = credits 比例 × 10 + idleWeight + successRate × 3
//
//	  - credits 比例 = 该号 credits / 候选集内最大 credits（避免量纲爆炸）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//	  - successRate = successCount/(successCount+errTotal)；无请求记录给 1.5（中性偏信任）
//
// credits 全 0 时仍按 idle+successRate 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeighted(cands []*entry) *entry {
	now := time.Now()
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i, e := range cands {
		w := p.weightOf(e, maxCredits, now)
		weights[i] = int64(w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// weightOf 计算单个账号的三因子权重。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 1b. 快过期积分加成：官方活动赠送的奖励积分按批过期，不用就作废。
	// creditsExpiring 占总量比例越高，越应优先被消耗——把"快过期占比"作为独立的
	// 强权重项（×expiringWeight），让快过期积分多的号优先选。与 credits 总量项
	// 正交：那是按总量，这是按过期紧迫度。
	if e.credits > 0 && e.creditsExpiring > 0 {
		w += float64(e.creditsExpiring) / float64(e.credits) * expiringWeight
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// 3. 成功率 ×3。
	totalReq := e.successCount + e.errTotal
	if totalReq > 0 {
		w += float64(e.successCount) / float64(totalReq) * 3
	} else {
		w += 1.5 // 无请求记录 → 中性偏信任
	}
	return w
}

// SetCredits 更新账号余额。

// expiryRank 到期紧迫度分级（**越小越先被选中**），口径与参考实现
// admin/credits.urgency_rank / pool.expiry_key 完全一致：
//
//	0 —— 有快过期窗口内的剩余额度（不烧就作废，最紧急）
//	1 —— 有明确到期时间但还早（自然形成"离到期近的先走"）
//	2 —— 长期有效（没有作废风险，只用它兜底）
//
// 数据来源是 creditsExpiring / creditsExpiringAt（由签到/余额刷新按上游
// ResourceDiag 同步写入）。缺数据（老 state、从未刷新过）→ rank 2：
// 把未知当成"没有作废风险"，最坏只是晚点用它；反过来把未知当成"快过期"，
// 会让调度去烧一个根本不紧急的号，耽误真正快作废的额度。调用方需已持 p.mu。
func (e *entry) expiryRank() int {
	if e.creditsExpiring > 0 {
		return 0
	}
	if !e.creditsExpiringAt.IsZero() {
		return 1
	}
	return 2
}

// expiringWeight 快过期积分占比的权重系数（三因子之外的第四因子）。
// 取 8：略低于 credits 总量项（×10），足以在"快过期多"与"总量相近"的号之间拉开差距，
// 又不至于压过总量项让"总量大但快过期少"的号被完全饿死。
const expiringWeight = 8.0
