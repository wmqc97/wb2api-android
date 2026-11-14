package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestWeightedPickPrefersExpiringCredits 到期紧迫度是**严格优先**（不是概率倾斜）：
// 只要有一个"窗口内到期且还有剩余"的账号，选号就必须落在它身上——哪怕它的余额
// 远小于另一个长期有效的账号。参考实现用同一口径（pool.expiry_key 作为首要键）。
//
// 改动前该断言必红：weighted 模式只有"快过期占比"的权重加成，余额 10 万的号
// 仍会经常被抽中，那批快到期额度就一直烧不完。
func TestWeightedPickPrefersExpiringCredits(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "rich-evergreen"})
	p.Add(&auth.Auth{UID: "small-expiring"})
	p.SetCredits("rich-evergreen", 100000, 100000)
	p.SetCredits("small-expiring", 100, 100)
	// 顺序说明：SetCredits 会清掉 creditsExpiringAt（新余额下旧到期依据失效），
	// 所以必须**先**写余额、**后**写分桶。
	p.SetCreditsExpiringAt("small-expiring", 100, time.Now().Add(72*time.Hour))

	for i := 0; i < 200; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatalf("第 %d 次选号返回 nil", i)
		}
		if a.UID != "small-expiring" {
			t.Fatalf("第 %d 次选到 %s：有快过期额度的账号未被严格优先", i, a.UID)
		}
	}
}

// TestWeightedPickZeroRegressionWithoutExpiry 没有任何到期信息时（全体 rank=2）
// 必须回到原行为：两个等条件账号都应被选中（过滤是恒等操作，不打散分布）。
func TestWeightedPickZeroRegressionWithoutExpiry(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 100, 100)
	p.SetCredits("b", 100, 100)

	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		if a := p.Pick(); a != nil {
			seen[a.UID]++
		}
	}
	if seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("无到期信息时选号被过度收窄：%v", seen)
	}
}

// TestExpiryRank 三级分档口径（0 快过期剩余 / 1 有到期时间 / 2 长期有效）。
func TestExpiryRank(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		expiring int64
		at       time.Time
		want     int
	}{
		{"窗口内还有剩余", 50, now.Add(48 * time.Hour), 0},
		{"无窗口内剩余但有到期时间", 0, now.Add(48 * time.Hour), 1}, // rank1：有到期时间但当前不紧急
		{"长期有效", 0, time.Time{}, 2},
	}
	for _, c := range cases {
		e := &entry{creditsExpiring: c.expiring, creditsExpiringAt: c.at}
		if got := e.expiryRank(); got != c.want {
			t.Errorf("%s: expiryRank=%d want %d", c.name, got, c.want)
		}
	}
}
