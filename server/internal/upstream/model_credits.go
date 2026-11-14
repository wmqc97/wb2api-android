package upstream

import (
	"sync"
	"time"
)

// 模型倍率旁表：realm → (模型 id → 倍率原文)。
//
// 为什么需要单独一张表：上游**不是每次都下发倍率**。同一个模型在不同账号、不同
// 探测轮次里，Credits 可能是 "x0.79"，也可能是空串（实测：CN 的 glm-5.2 有 x0.79，
// 而 gpt-5.6-* 这类只在某些账号目录里出现且不带 credits）。倍率是**模型属性**
// （官方牌价），不是账号属性，所以把历史观测到的值攒起来，下次探测缺字段时回填——
// 否则 /v1/models 的 name 里只剩裸模型名，用户看不出这个模型要花多少积分。
//
// 按 realm 分表是必须的：同一 id 在两个域是**不同价**（实测 deepseek-v4.1-flash
// CN x0.11 / global x0.00），混在一起会把 CN 的号按国际版牌价展示。
//
// 生命周期：进程内、只增不减（只记录非空值，空值不覆盖旧值）。目录刷新时整体替换
// 的是 catalog 缓存，本表刻意不跟着清——「曾经观测到的牌价」比「这一轮没给」更有用，
// 也与上游 ModelRate 倍率表跨刷新持久的做法一致。
// modelCreditTTL 倍率观测有效期：与 /v1/models 目录缓存（10min）一致。
// 过期的观测就是"旧价"，上游调价后拿它回填 name 会显示错误倍率——宁可不显示。
const modelCreditTTL = 10 * time.Minute

type creditObservation struct {
	value string
	at    time.Time
}

var modelCreditsTable = struct {
	sync.RWMutex
	byRealm map[string]map[string]creditObservation
}{byRealm: map[string]map[string]creditObservation{}}

// RecordModelCredits 把本轮目录里出现的非空倍率并入旁表（空值不覆盖旧值）。
func RecordModelCredits(realm string, infos []ModelInfo) {
	if realm != "cn" && realm != "global" || len(infos) == 0 {
		return
	}
	modelCreditsTable.Lock()
	defer modelCreditsTable.Unlock()
	tbl := modelCreditsTable.byRealm[realm]
	if tbl == nil {
		tbl = make(map[string]creditObservation, len(infos))
		modelCreditsTable.byRealm[realm] = tbl
	}
	now := time.Now()
	for _, mi := range infos {
		if mi.ID == "" || mi.Credits == "" {
			continue
		}
		tbl[mi.ID] = creditObservation{value: mi.Credits, at: now}
	}
}

// ModelCreditsFor 查历史上观测到的倍率（realm 精确匹配；从未观测过 → ok=false）。
//
// 调用方（/v1/models 组装、面板展示）只在「本轮上游没给倍率」时用它回填，
// 上游值永远优先。
func ModelCreditsFor(realm, id string) (string, bool) {
	if id == "" {
		return "", false
	}
	modelCreditsTable.RLock()
	defer modelCreditsTable.RUnlock()
	obs, ok := modelCreditsTable.byRealm[realm][id]
	if !ok || time.Since(obs.at) > modelCreditTTL {
		return "", false // 过期观测 = 没有，不用旧价凑数
	}
	return obs.value, true
}

// resetModelCreditsForTest 清空旁表（仅测试用：全局状态会跨用例串扰）。
func resetModelCreditsForTest() {
	modelCreditsTable.Lock()
	modelCreditsTable.byRealm = map[string]map[string]creditObservation{}
	modelCreditsTable.Unlock()
}
