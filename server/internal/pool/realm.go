// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"time"
)

// HasRealm 报告池中是否存在该 realm 的账号（按归属判，不看冷却/禁用/健康度）。
//
// 与 AvailableUIDsForRealm 的分工：那个回答"现在能不能调"，本方法回答"这个域有没有
// 账号"——用于裸模型名的域归属决策（模型列表列哪些域、裸名路由去哪个域）。用归属
// 而非健康度是为了让决策稳定：账号全在冷却时列表不应突然整个空掉。
func (p *Pool) HasRealm(realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.byUID {
		if e.a.Realm() == realm {
			return true
		}
	}
	return false
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。排序口径同 AvailableUIDs
// （sortByOrderLocked → Pool.Order()），域过滤保序。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	p.sortByOrderLocked(uids)
	return uids
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
// 排序口径同 AvailableUIDs（sortByOrderLocked → Pool.Order()），域过滤/健康过滤保序。
// 保留积分闸门同 AvailableUIDsForModel（余额触底的号对贵模型不列入）——粘性分配的
// 候选集与选号口径必须一致，否则会话会被绑到池侧随后拒绝的号上。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	gate := p.reserveGateFor(model, false)
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if !gate.allows(e, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	p.sortByOrderLocked(uids)
	return uids
}

// HasUsableRealm 报告该域是否存在**可用**账号（未禁用、未临时停用）。
//
// 与 HasRealm 的分工：HasRealm 只回答「有没有这个域的账号」，用于路由/列表口径；
// 本方法回答「这个域现在有没有能干活的人」，用于面板默认域这类**交互**决策——
// 按 HasRealm 判会出现「global 账号全被停用/烧光，打开模型页直接报错」的体验，
// 而用户其实只想看 CN 的模型列表。
func (p *Pool) HasUsableRealm(realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.byUID {
		if e.disabled || e.manualDisabled {
			continue
		}
		if e.a.Realm() == realm {
			return true
		}
	}
	return false
}
