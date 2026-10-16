package panel

import (
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Credentials are already persisted and in the pool. Network maintenance must
// not delay the import/login receipt; at most two accounts run concurrently.
func (p *Panel) queueAccountSetup(a *auth.Auth) {
	if p.cfg.Upstream == nil {
		return
	}
	p.setupMu.Lock()
	defer p.setupMu.Unlock()
	if p.setupPending == nil {
		p.setupPending = make(map[string]bool)
	}
	if p.setupPending[a.UID] {
		return
	}
	p.setupPending[a.UID] = true
	p.setupQueue = append(p.setupQueue, a)
	if p.setupWorkers < 2 {
		p.setupWorkers++
		go p.runAccountSetup()
	}
}

func (p *Panel) runAccountSetup() {
	for {
		p.setupMu.Lock()
		if len(p.setupQueue) == 0 {
			p.setupWorkers--
			p.setupMu.Unlock()
			return
		}
		a := p.setupQueue[0]
		p.setupQueue[0] = nil
		p.setupQueue = p.setupQueue[1:]
		p.setupMu.Unlock()
		// Reimports may replace the credentials while an entry is queued.
		if current := p.cfg.Pool.AuthByUID(a.UID); current != nil {
			p.completeAccountSetup(current)
		}
		p.setupMu.Lock()
		delete(p.setupPending, a.UID)
		p.setupMu.Unlock()
	}
}

func (p *Panel) completeAccountSetup(a *auth.Auth) {
	uid := logfmt.UID8(a.UID)
	if auth.ResolveRealm(a.RealmStored(), a.DomainValue()) == "global" {
		if _, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
			log.Printf("panel: background registration uid=%s: %v", uid, err)
		}
		if _, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
			log.Printf("panel: background trial uid=%s: %v", uid, err)
		}
	} else if !a.IsEnterprise() {
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			log.Printf("panel: background checkin uid=%s: %v", uid, err)
		}
	}
	if rm, tt, exp, diag, err := p.cfg.Upstream.UserResourceDetailedDiag(a, p.expiringSoonWindow()); err == nil {
		if p.cfg.Pool.AuthByUID(a.UID) == a {
			p.cfg.Pool.ReenableIfCredits(a.UID, rm, tt)
			p.cfg.Pool.SetCreditsExpiringAt(a.UID, exp, diag.ExpiringEnd)
		}
		log.Printf("panel: background balance uid=%s %s", uid, upstream.BalanceLine(rm, tt, exp, diag))
	} else {
		log.Printf("panel: background balance uid=%s: %v", uid, err)
	}
}
