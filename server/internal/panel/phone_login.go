// phone_login.go 面板内嵌的手机号 + 短信验证码登录（WorkBuddy CN / OneID 短信）。
//
// 为什么需要
// ---------
// 原有的「浏览器登录」（login.go）走官方设备授权：把用户丢到官方授权页，登录完
// 靠 workbuddy:// 之类的私有协议回调 —— 手机浏览器（尤其中文 App 内置 WebView）
// 根本走不到底。本文件把整条链路改成**纯 HTTP**，在面板弹窗里直接输入手机号、
// 收验证码、提交，全程不跳转，换到的凭证与桌面端同构，直接热加载进号池。
//
// 链路（每一步都在本服务端发生；移植自参考实现的 admin/wb_login.py）
// --------------------------------------------------------------
//
//	 1  POST /v2/plugin/auth/state?platform=workbuddy   → 设备授权 state（换终端凭证的钥匙）
//	 2  GET  /console/auth/login?...&state=<plugin>      → 302 到 Keycloak，下发 console 会话 Cookie
//	 3  GET  <Keycloak authorize>                        → 登录页 HTML 内嵌 broker/oneid 链接
//	 4  GET  <broker/oneid/login>                        → 303 到 OneID /v1/authorize（带 state S1）
//	 5  GET  <OneID /v1/authorize>                       → 302 到 account.tencent.com（建 OneID 会话）
//	 6  POST oauth2.account.tencent.com/v1/auth/sms/code/send    → 发短信，返回一次性 state_token
//	 7  POST .../v1/auth/sms/code/verify                  → 校验验证码（沿用同一个 state_token）
//	 8  GET  .../v1/auth/accounts?state_token=            → 账号列表 + OneID 授权 code
//	 9  GET  <broker/oneid/endpoint?code=&state=S1>       → Keycloak 建立真的 console 会话
//	10  GET  /console/auth/login?...&state=<plugin>       → 把 console 会话绑到设备 state
//	11  GET  /v2/plugin/auth/token?state=<plugin>         → accessToken / refreshToken / domain
//
// 关键坑（改动前务必看）
// ---------------------
//   - 第 8 步**只能用一次**：/v1/auth/accounts 与 /v1/auth/select_account 都会消耗
//     state_token，先调 accounts 再调 select_account 必得 E0010072「无效的token」。
//     正确做法：accounts 一次拿到 code；没拿到 code 才退回 select_account。
//   - 跨域 Cookie 必须留在**同一个 jar**：OneID 会话横跨 workbuddy.cn 与
//     oauth2.account.tencent.com，拆成两个 client 会静默丢会话（表现为最后一步
//     一直 11217 login ing...）。
//   - 第 9 步的 state 参数是 OneID 的 S1（第 4 步那个），不是 Keycloak 的 session_code。
//   - 第 10 步不做的话，第 11 步会一直回 11217「login ing...」。
//   - X-Device-Token（腾讯风控指纹）**不是必需**：缺失时上游走「未配置」降级路径，
//     短信照常下发。故本实现不依赖任何浏览器指纹。
//
// 边界
// ----
// 验证码始终由腾讯下发给用户本人，本服务不接触用户手机、不绕过任何验证；
// 上游按号风控要求图形验证码时如实报 need_captcha（纯协议过不去滑块）。
package panel

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// phoneWebUA 浏览器 UA：登录链路全程走 Web 形态，不能用桌面端 CLI UA
	// （CLI UA 在 console 授权链上会被引到设备授权分支）。
	phoneWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36 Edg/149.0.0.0"

	phoneSessionTTL    = 15 * time.Minute // 一次登录会话的有效期
	phoneConsumedGrace = 60 * time.Second // 已成功会话的保留宽限期（双击幂等）
	phoneMaxSessions   = 200              // 会话表上限（每个会话含连接池，是有成本的资源）
	phoneHTTPTimeout   = 15 * time.Second
	phoneMaxHops       = 12
	phoneMaxConcurrent = 8 // 在途上游登录请求上限（超出的立刻回「繁忙」，不排队耗线程）

	// 发码节流：上游按**号**风控，这里按号 + 按 IP 双重节流，防止被拿来刷短信。
	phoneSMSCooldown   = 60 * time.Second // 同一手机号两次发码的最小间隔
	phoneSMSMaxPerHour = 10               // 同一客户端 IP 每小时的发码次数上限
	phoneSMSExitGap    = 5 * time.Second  // 全局面最小间隔（挡住连点/脚本重放）
)

// 上游域。用变量而非常量：单元测试可用 httptest 覆盖整条链路。
var (
	phoneHost   = "https://www.workbuddy.cn"
	phoneOAuth2 = "https://oauth2.account.tencent.com"

	// phoneAllowedHosts 跟随重定向的白名单（防止被引到站外）。
	phoneAllowedHosts = []string{"workbuddy.cn", "codebuddy.cn", "tencent.com"}
)

// phoneError 可预期失败：message 给人看，step/reason 给前端做流程判断
// （reason=need_captcha 时前端提示换号，而不是把上游中文错误原样怼给用户）。
type phoneError struct {
	message string
	step    string
	reason  string
	retry   int // 秒；>0 时前端据此做倒计时
}

func (e *phoneError) Error() string { return e.message }

func phoneFail(step, format string, args ...any) *phoneError {
	return &phoneError{message: fmt.Sprintf(format, args...), step: step}
}

// ── 会话 ─────────────────────────────────────────────────────────────

// phoneSession 一次「手机号换凭证」的完整状态。
//
// 每个会话持有**自己的 http.Client（含 cookiejar）** —— 这是必须的：OneID 的登录态
// 横跨 workbuddy.cn 与 oauth2.account.tencent.com，共用 client 会把不同用户的会话
// 串在一起（并发登录时表现为串号）。
type phoneSession struct {
	sid     string
	cli     *http.Client
	jar     *phoneJar
	created time.Time

	pluginState   string // 设备授权 state
	pluginAuthURL string
	kcURL         string // Keycloak 授权地址
	brokerURL     string // broker/oneid/login 地址
	oneidState    string // OneID CSRF state（回传 code 时要用）

	smsToken   string // 短信一次性 state_token
	mobile     string // 最近一次发码的号码（校验时比对，防串号）
	country    string
	codeSentAt time.Time
	consumed   bool
	consumedAt time.Time

	mu     sync.Mutex
	result *phoneResult // 已成功换取的凭证（幂等复用）
}

// phoneResult 一次成功登录的结果（缓存给重复提交复用）。
type phoneResult struct {
	uid          string
	nickname     string
	realm        string
	enterpriseID string
	token        phoneToken
	duplicated   bool
}

var (
	phoneMu       sync.Mutex
	phoneSessions = map[string]*phoneSession{}
	phoneGate     = make(chan struct{}, phoneMaxConcurrent)
)

// phoneAcquireGate 占用一个上游名额；满了立刻返回 false（不排队）。
func phoneAcquireGate() bool {
	select {
	case phoneGate <- struct{}{}:
		return true
	default:
		return false
	}
}

func phoneReleaseGate() {
	select {
	case <-phoneGate:
	default:
	}
}

func phoneRandomSID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("p%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// phoneSweepLocked 回收过期/已消费的会话（调用方须持 phoneMu）。
// 已消费的会话不立即删：留一个短宽限期，让双击的第二个请求能复用第一个的成功结果。
func phoneSweepLocked(now time.Time) {
	for sid, s := range phoneSessions {
		expired := now.Sub(s.created) > phoneSessionTTL
		done := s.consumed && now.Sub(s.consumedAt) > phoneConsumedGrace
		if expired || done {
			delete(phoneSessions, sid)
			s.cli.CloseIdleConnections()
		}
	}
}

// phoneRegister 把会话放进表里并强制数量上限（公开面板必须假设会被滥用）。
func phoneRegister(s *phoneSession) error {
	phoneMu.Lock()
	defer phoneMu.Unlock()
	now := time.Now()
	phoneSweepLocked(now)
	if len(phoneSessions) >= phoneMaxSessions {
		// 腾地方：先收掉已消费的（不等宽限期）。
		for sid, old := range phoneSessions {
			if old.consumed {
				delete(phoneSessions, sid)
				old.cli.CloseIdleConnections()
			}
		}
	}
	if len(phoneSessions) >= phoneMaxSessions {
		return phoneFail("busy", "当前登录人数较多，请稍后重试")
	}
	phoneSessions[s.sid] = s
	return nil
}

func phoneGetSession(sid string) (*phoneSession, error) {
	phoneMu.Lock()
	now := time.Now()
	phoneSweepLocked(now)
	s := phoneSessions[sid]
	phoneMu.Unlock()
	if s == nil {
		return nil, phoneFail("session", "登录会话不存在或已过期，请重新获取验证码")
	}
	if now.Sub(s.created) > phoneSessionTTL {
		return nil, phoneFail("session", "登录会话已过期，请重新获取验证码")
	}
	return s, nil
}

func phoneDropSession(sid string) {
	phoneMu.Lock()
	s := phoneSessions[sid]
	delete(phoneSessions, sid)
	phoneMu.Unlock()
	if s != nil {
		s.cli.CloseIdleConnections()
	}
}

// ── HTTP 小工具 ───────────────────────────────────────────────────────

// phoneJar 记录型 cookie jar：在标准 jar 之上按名字记下所有见过的 cookie。
//
// 为什么需要：Keycloak 的 KEYCLOAK_SESSION / KEYCLOAK_IDENTITY 带
// Path=/auth/realms/<realm>/，而 http.CookieJar.Cookies(u) 只返回**路径匹配**的
// cookie —— 用根路径去查会得到空集，于是「登录其实成功了」会被误判成失败
// （参考实现直接读整个 cookie jar，本类型等价还原这个语义）。
type phoneJar struct {
	inner http.CookieJar
	mu    sync.Mutex
	seen  map[string]string
}

func (j *phoneJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.inner.SetCookies(u, cookies)
	j.mu.Lock()
	for _, c := range cookies {
		if c.MaxAge < 0 { // 删除指令
			delete(j.seen, c.Name)
			continue
		}
		j.seen[c.Name] = c.Value
	}
	j.mu.Unlock()
}

func (j *phoneJar) Cookies(u *url.URL) []*http.Cookie { return j.inner.Cookies(u) }

// has 报告是否见过该名字的 cookie（不看路径，见类型注释）。
func (j *phoneJar) has(name string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.seen[name]
	return ok
}

func (j *phoneJar) names() map[string]bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make(map[string]bool, len(j.seen))
	for name := range j.seen {
		out[name] = true
	}
	return out
}

func phoneNewClient() (*http.Client, *phoneJar) {
	inner, _ := cookiejar.New(nil)
	jar := &phoneJar{inner: inner, seen: map[string]string{}}
	return &http.Client{
		Timeout: phoneHTTPTimeout,
		Jar:     jar,
		// 手动跟随：只跳白名单域，且每一跳都能记录 Referer（自动跟随会丢）。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, jar
}

func phoneDomain() string {
	if u, err := url.Parse(phoneHost); err == nil && u.Host != "" {
		return u.Host
	}
	return "www.workbuddy.cn"
}

func (s *phoneSession) headers(referer, accept string, extra map[string]string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", phoneWebUA)
	h.Set("Accept-Language", "zh-CN,zh;q=0.9")
	h.Set("Accept", accept)
	h.Set("Origin", phoneHost)
	if referer == "" {
		referer = phoneHost + "/"
	}
	h.Set("Referer", referer)
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

// do 发一次请求并读全响应体（≤1 MiB）。不跟随重定向（见 phoneNewClient）。
func (s *phoneSession) do(method, fullURL string, body []byte, referer, accept string, extra map[string]string) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, fullURL, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header = s.headers(referer, accept, extra)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.cli.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, raw, nil
}

func phoneHostAllowed(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, allowed := range phoneAllowedHosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

func phoneResolve(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// follow 跟随重定向链，只跳白名单域（上游会依次甩到 keycloak → OneID → console）。
func (s *phoneSession) follow(start, referer string) (*http.Response, []byte, error) {
	cur, ref := start, referer
	var resp *http.Response
	var raw []byte
	for hop := 0; hop < phoneMaxHops; hop++ {
		r, b, err := s.do(http.MethodGet, cur, nil, ref, "text/html,application/json,*/*", nil)
		if err != nil {
			return nil, nil, err
		}
		resp, raw = r, b
		loc := r.Header.Get("Location")
		if r.StatusCode < 300 || r.StatusCode > 399 || loc == "" {
			return resp, raw, nil
		}
		next := phoneResolve(cur, loc)
		if !phoneHostAllowed(next) {
			return resp, raw, nil
		}
		ref, cur = cur, next
	}
	return resp, raw, nil
}

// phoneJSON 解上游 JSON 对象；非 JSON（风控拦截页/网关错误页）给可读错误。
func phoneJSON(raw []byte, step string) (map[string]any, *phoneError) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		head := string(raw)
		if len(head) > 160 {
			head = head[:160]
		}
		return nil, phoneFail(step, "上游返回不是 JSON（可能被风控拦截）：%s", strings.TrimSpace(head))
	}
	return obj, nil
}

// phoneErrCode 把 errCode 字段（数字或字符串）归一化成字符串；缺省返回空串。
func phoneErrCode(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case float64:
		return fmt.Sprintf("%.0f", t)
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}

func phoneNonEmpty(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// phoneEnvelopeData 解 {code,msg,data} 信封；code 非 0 抛错。
func phoneEnvelopeData(raw []byte, step string) (map[string]any, *phoneError) {
	obj, perr := phoneJSON(raw, step)
	if perr != nil {
		return nil, perr
	}
	if code := phoneErrCode(obj["code"]); code != "" && code != "0" {
		msg := phoneNonEmpty(obj["msg"])
		if msg == "" {
			msg = "上游返回 code=" + code
		}
		return nil, phoneFail(step, "%s", msg)
	}
	data, _ := obj["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// ── 第 1-5 步：建会话，把授权链铺到 OneID ─────────────────────────────

func phoneStartSession() (*phoneSession, error) {
	cli, jar := phoneNewClient()
	s := &phoneSession{sid: phoneRandomSID(), cli: cli, jar: jar, created: time.Now()}
	domain := phoneDomain()
	fail := func(err error) (*phoneSession, error) {
		s.cli.CloseIdleConnections()
		return nil, err
	}

	// 1) 设备授权 state（终端凭证就靠它换取）
	resp, raw, err := s.do(http.MethodPost,
		phoneHost+"/v2/plugin/auth/state?platform=workbuddy",
		[]byte("{}"), "",
		"application/json, text/plain, */*",
		map[string]string{"X-Requested-With": "XMLHttpRequest"})
	if err != nil {
		return fail(phoneFail("auth_state", "网络错误：%v", err))
	}
	if resp.StatusCode >= 300 {
		return fail(phoneFail("auth_state", "上游返回 HTTP %d", resp.StatusCode))
	}
	data, perr := phoneEnvelopeData(raw, "auth_state")
	if perr != nil {
		return fail(perr)
	}
	s.pluginState = phoneNonEmpty(data["state"])
	s.pluginAuthURL = phoneNonEmpty(data["authUrl"])
	if s.pluginState == "" {
		return fail(phoneFail("auth_state", "上游未返回设备授权 state"))
	}

	// 2) /console/auth/login → Keycloak（同时下发 console 会话 Cookie）
	q := url.Values{}
	q.Set("platform", "workbuddy")
	q.Set("state", s.pluginState)
	q.Set("domain", domain)
	resp, _, err = s.do(http.MethodGet, phoneHost+"/console/auth/login?"+q.Encode(), nil, "",
		"application/json, text/plain, */*", map[string]string{"X-Domain": domain})
	if err != nil {
		return fail(phoneFail("console_auth_login", "网络错误：%v", err))
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return fail(phoneFail("console_auth_login", "未能取得 Keycloak 授权地址（上游可能已调整入口）"))
	}
	s.kcURL = phoneResolve(phoneHost, loc)

	// 3) Keycloak 登录页：HTML 里内嵌 broker/oneid 链接（服务端渲染，无需 JS）
	_, raw, err = s.do(http.MethodGet, s.kcURL, nil, "", "text/html,*/*", nil)
	if err != nil {
		return fail(phoneFail("keycloak_page", "网络错误：%v", err))
	}
	m := phoneOneIDLink.FindSubmatch(raw)
	if m == nil {
		return fail(phoneFail("keycloak_page", "登录页未提供 OneID 入口（上游可能已调整页面）"))
	}
	s.brokerURL = phoneResolve(phoneHost, strings.ReplaceAll(string(m[1]), "&amp;", "&"))

	// 4) broker/oneid/login → OneID /v1/authorize（拿 S1）
	resp, _, err = s.do(http.MethodGet, s.brokerURL, nil, s.kcURL, "text/html,*/*", nil)
	if err != nil {
		return fail(phoneFail("broker_login", "网络错误：%v", err))
	}
	authorize := resp.Header.Get("Location")
	if authorize == "" {
		return fail(phoneFail("broker_login", "OneID 未返回授权地址"))
	}
	if u, err := url.Parse(authorize); err == nil {
		s.oneidState = u.Query().Get("state")
	}
	if s.oneidState == "" {
		return fail(phoneFail("broker_login", "OneID 授权地址缺少 state"))
	}

	// 5) 走一趟 OneID 首页，建立 OneID 侧会话。失败不致命（有的账号已有会话），
	//    后面发码会再校验。
	if r5, _, err := s.do(http.MethodGet, authorize, nil, s.kcURL, "text/html,*/*", nil); err != nil {
		log.Printf("panel: 手机号登录 OneID 预访问失败（忽略）：%v", err)
	} else if r5.StatusCode >= 400 {
		log.Printf("panel: 手机号登录 OneID 预访问返回 %d（忽略）", r5.StatusCode)
	}

	if err := phoneRegister(s); err != nil {
		s.cli.CloseIdleConnections()
		return nil, err
	}
	return s, nil
}

// phoneOneIDLink 登录页里的 OneID 入口（data-idp="oneid" 后的 href）。
var phoneOneIDLink = regexp.MustCompile("data-idp=\"oneid\"[^>]*href=\"([^\"]+)\"")

// ── 手机号归一化 ──────────────────────────────────────────────────────

// phoneMobilePatterns 支持的地区（面板只放开 +86/+852/+853，避免把国际号写死错）。
var phoneMobilePatterns = map[string]*regexp.Regexp{
	"86":  regexp.MustCompile("^1[3-9][0-9]{9}$"),
	"852": regexp.MustCompile("^[4-9][0-9]{7}$"),
	"853": regexp.MustCompile("^6[0-9]{7}$"),
}

// phoneLocalDigits 各地区号码位数（归一化时判断要不要剥国家码）。
var phoneLocalDigits = map[string]int{"86": 11, "852": 8, "853": 8}

// phoneNormalizeMobile 归一化手机号：去空格/括号/连字符，剥掉 +86 前缀，按地区校验。
func phoneNormalizeMobile(mobile, country string) (string, string, *phoneError) {
	country = strings.TrimPrefix(strings.TrimSpace(country), "+")
	if country == "" {
		country = "86"
	}
	pat, ok := phoneMobilePatterns[country]
	if !ok {
		return "", "", phoneFail("mobile", "不支持的地区代码 +%s", country)
	}
	digits := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(strings.TrimSpace(mobile))
	digits = strings.TrimPrefix(digits, "+"+country)
	if strings.HasPrefix(digits, country) && len(digits) == len(country)+phoneLocalDigits[country] {
		digits = digits[len(country):]
	}
	if !pat.MatchString(digits) {
		return "", "", phoneFail("mobile", "请输入所选地区的有效手机号")
	}
	return digits, country, nil
}

// ── 发码节流 ──────────────────────────────────────────────────────────

var phoneQuota = struct {
	mu       sync.Mutex
	byIP     map[string][]time.Time
	byMobile map[string]time.Time
	exitLast time.Time
}{byIP: map[string][]time.Time{}, byMobile: map[string]time.Time{}}

// phoneCheckQuota 发码节流。超限返回带 retry 秒数的 phoneError（前端做倒计时）。
//
// 三层：同一号码 60s、同一 IP 每小时 10 次、全局最小间隔（挡连点/脚本重放）。
// 上游是按**号**风控的（同一号码短时间多次发码会要图形验证码），所以按号这一层
// 是硬需求；按 IP 那层防的是「拿到面板地址的人拿我们的出口 IP 刷短信」。
func phoneCheckQuota(ip, mobile string) *phoneError {
	now := time.Now()
	phoneQuota.mu.Lock()
	defer phoneQuota.mu.Unlock()

	if wait := phoneQuota.exitLast.Add(phoneSMSExitGap).Sub(now); wait > 0 {
		e := phoneFail("sms_send", "操作过于频繁，请 %d 秒后再试", int(wait.Seconds())+1)
		e.retry = int(wait.Seconds()) + 1
		return e
	}
	if last, ok := phoneQuota.byMobile[mobile]; ok {
		if wait := last.Add(phoneSMSCooldown).Sub(now); wait > 0 {
			e := phoneFail("sms_send", "该手机号发送过于频繁，请 %d 秒后再试", int(wait.Seconds())+1)
			e.retry = int(wait.Seconds()) + 1
			return e
		}
	}
	hits := phoneQuota.byIP[ip][:0]
	for _, t := range phoneQuota.byIP[ip] {
		if now.Sub(t) < time.Hour {
			hits = append(hits, t)
		}
	}
	if len(hits) >= phoneSMSMaxPerHour {
		e := phoneFail("sms_send", "本小时发送次数已达上限，请稍后再试")
		e.retry = 60
		phoneQuota.byIP[ip] = hits
		return e
	}

	hits = append(hits, now)
	phoneQuota.byIP[ip] = hits
	phoneQuota.byMobile[mobile] = now
	phoneQuota.exitLast = now
	// 顺手清理过期条目，避免长期运行后 map 只增不减。
	for key, ts := range phoneQuota.byIP {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > time.Hour {
			delete(phoneQuota.byIP, key)
		}
	}
	for key, ts := range phoneQuota.byMobile {
		if now.Sub(ts) > time.Hour {
			delete(phoneQuota.byMobile, key)
		}
	}
	return nil
}

// ── 第 6 步：发短信 ───────────────────────────────────────────────────

func (s *phoneSession) sendSMS(mobile, country string) (int, error) {
	if s.consumed {
		return 0, phoneFail("session", "该会话已完成登录，请重新获取验证码")
	}
	if s.smsToken != "" && (s.mobile != mobile || s.country != country) {
		return 0, phoneFail("mobile", "当前会话正在等待验证码，请完成验证后再更换号码")
	}
	body, _ := json.Marshal(map[string]any{
		"client_code": "codebuddy",
		"mobile":      "+" + country + " " + mobile,
		"scopes":      []string{"openid", "mobile", "profile"},
	})
	resp, raw, err := s.do(http.MethodPost, phoneOAuth2+"/v1/auth/sms/code/send", body, "",
		"application/json, text/plain, */*",
		map[string]string{"X-TOA-LANG": "zh-CN"})
	if err != nil {
		return 0, phoneFail("sms_send", "网络错误：%v", err)
	}
	if resp.StatusCode >= 500 {
		return 0, phoneFail("sms_send", "上游暂时不可用（HTTP %d），请稍后再试", resp.StatusCode)
	}
	obj, perr := phoneJSON(raw, "sms_send")
	if perr != nil {
		return 0, perr
	}
	if code := phoneErrCode(obj["errCode"]); code != "" && code != "0" {
		msg := phoneNonEmpty(obj["errMessage"])
		if msg == "" {
			msg = "发送验证码失败"
		}
		return 0, phoneFail("sms_send", "%s", msg)
	}
	token := phoneNonEmpty(obj["state_token"])
	if token == "" {
		// 上游按号风控要求图形验证码：纯协议过不去滑块，只能如实告知（换号可解）。
		if obj["captcha"] != nil || phoneNonEmpty(obj["status"]) == "need_captcha" {
			e := phoneFail("sms_send", "该手机号短时间内请求验证码次数过多，需要完成安全验证；请稍后再试或改用其他手机号")
			e.reason = "need_captcha"
			return 0, e
		}
		return 0, phoneFail("sms_send", "上游未返回 state_token")
	}
	s.smsToken = token
	s.mobile = mobile
	s.country = country
	s.codeSentAt = time.Now()
	expires := 60
	if n, ok := obj["expires_in"].(float64); ok && n > 0 {
		expires = int(n)
	}
	log.Printf("panel: 手机号登录已发验证码 sid=%s mobile=%s****", s.sid[:8], mobile[:3])
	return expires, nil
}

// ── 第 7-11 步：校验 → 换 console 会话 → 换凭证 ──────────────────────

func (s *phoneSession) verifyCode(code string) (*phoneResult, error) {
	// 7) 校验验证码
	body, _ := json.Marshal(map[string]any{"state_token": s.smsToken, "code": code})
	resp, raw, err := s.do(http.MethodPost, phoneOAuth2+"/v1/auth/sms/code/verify", body, "",
		"application/json, text/plain, */*",
		map[string]string{"X-TOA-LANG": "zh-CN"})
	if err != nil {
		return nil, phoneFail("sms_verify", "网络错误：%v", err)
	}
	if resp.StatusCode >= 500 {
		return nil, phoneFail("sms_verify", "上游暂时不可用（HTTP %d），请稍后再试", resp.StatusCode)
	}
	obj, perr := phoneJSON(raw, "sms_verify")
	if perr != nil {
		return nil, perr
	}
	if ec := phoneErrCode(obj["errCode"]); ec != "" && ec != "0" {
		msg := phoneNonEmpty(obj["errMessage"])
		if msg == "" {
			msg = "验证码校验失败"
		}
		return nil, phoneFail("sms_verify", "%s", msg)
	}
	// 校验成功后会回一个新的 state_token（实测同值）；以返回值为准更稳。
	if tok := phoneNonEmpty(obj["state_token"]); tok != "" {
		s.smsToken = tok
	}

	// 8) 取账号 + OneID 授权 code。这一步会消耗 state_token，只能做一次。
	oneidCode, perr := s.fetchOneIDCode()
	if perr != nil {
		return nil, perr
	}

	// 9-10) 用 code 完成 broker 登录，并把 console 会话绑到设备 state
	if perr := s.exchangeCodeForSession(oneidCode); perr != nil {
		return nil, perr
	}
	s.bindDeviceState()

	// 11) 换取凭证 + 账号信息
	tok, perr := s.fetchCredential()
	if perr != nil {
		return nil, perr
	}
	acct, _ := s.fetchAccount(tok.accessToken, tok.domain)

	uid := phoneNonEmpty(acct["uid"])
	if uid == "" {
		return nil, phoneFail("auth_token", "上游未返回账号 uid（凭证无法入池）")
	}
	if !validUID(uid) {
		return nil, phoneFail("auth_token", "上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）")
	}
	nick := phoneNonEmpty(acct["nickname"])
	if nick == "" || strings.EqualFold(nick, "null") {
		nick = phoneNonEmpty(acct["phoneNumber"])
	}
	return &phoneResult{
		uid:          uid,
		nickname:     nick,
		realm:        "cn",
		enterpriseID: phoneNonEmpty(acct["enterpriseId"]),
		token:        tok,
	}, nil
}

// fetchOneIDCode 第 8 步：/v1/auth/accounts 拿授权 code（拿不到才退回 select_account）。
func (s *phoneSession) fetchOneIDCode() (string, *phoneError) {
	q := url.Values{}
	q.Set("state_token", s.smsToken)
	_, raw, err := s.do(http.MethodGet, phoneOAuth2+"/v1/auth/accounts?"+q.Encode(), nil, "",
		"application/json, text/plain, */*", nil)
	if err != nil {
		return "", phoneFail("account_select", "网络错误：%v", err)
	}
	obj, perr := phoneJSON(raw, "account_select")
	if perr != nil {
		return "", perr
	}
	if code := phoneErrCode(obj["errCode"]); code == "" || code == "0" {
		if c := phoneNonEmpty(obj["code"]); c != "" {
			return c, nil
		}
	}

	// 退路：accounts 没给 code 时用 select_account 再试一次（仅当 state_token 未被消耗）。
	accID := ""
	if accounts, ok := obj["accounts"].([]any); ok {
		for _, it := range accounts {
			if m, ok := it.(map[string]any); ok {
				if id := phoneNonEmpty(m["id"]); id != "" {
					accID = id
					break
				}
			}
		}
	}
	body, _ := json.Marshal(map[string]any{"state_token": s.smsToken, "account_id": orDefault(accID, "codebuddy@")})
	_, raw2, err := s.do(http.MethodPost, phoneOAuth2+"/v1/auth/select_account", body, "",
		"application/json, text/plain, */*", nil)
	if err != nil {
		return "", phoneFail("account_select", "网络错误：%v", err)
	}
	obj2, perr := phoneJSON(raw2, "account_select")
	if perr != nil {
		return "", perr
	}
	if ec := phoneErrCode(obj2["errCode"]); ec != "" && ec != "0" {
		msg := phoneNonEmpty(obj2["errMessage"])
		if msg == "" {
			msg = phoneNonEmpty(obj["errMessage"])
		}
		if msg == "" {
			msg = "账号选择失败"
		}
		return "", phoneFail("account_select", "%s", msg)
	}
	if c := phoneNonEmpty(obj2["code"]); c != "" {
		return c, nil
	}
	return "", phoneFail("account_select", "未能取得 OneID 授权码")
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// exchangeCodeForSession 第 9 步：把 OneID 授权 code 交给 Keycloak broker。
// 成功的判据是 cookie（KEYCLOAK_SESSION/KEYCLOAK_IDENTITY），不是响应体。
func (s *phoneSession) exchangeCodeForSession(code string) *phoneError {
	endpoint := fmt.Sprintf("%s/auth/realms/copilot/broker/oneid/endpoint?code=%s&state=%s",
		phoneHost, url.QueryEscape(code), url.QueryEscape(s.oneidState))
	_, raw, err := s.follow(endpoint, s.kcURL)
	if err != nil {
		return phoneFail("broker_endpoint", "网络错误：%v", err)
	}
	names := s.cookieNames()
	if !names["KEYCLOAK_SESSION"] && !names["KEYCLOAK_IDENTITY"] {
		head := string(raw)
		if len(head) > 160 {
			head = head[:160]
		}
		return phoneFail("broker_endpoint", "未能建立登录会话（上游可能拒绝了本次授权）：%s", strings.TrimSpace(head))
	}
	return nil
}

// cookieNames 本次会话见过的全部 cookie 名（不看路径，理由见 phoneJar）。
func (s *phoneSession) cookieNames() map[string]bool {
	if s.jar == nil {
		return map[string]bool{}
	}
	return s.jar.names()
}

// bindDeviceState 第 10 步：让 console 会话认领设备 state。
// 不做这一步，第 11 步会一直回 11217「login ing...」。
func (s *phoneSession) bindDeviceState() {
	q := url.Values{}
	q.Set("platform", "workbuddy")
	q.Set("state", s.pluginState)
	q.Set("domain", phoneDomain())
	if _, _, err := s.follow(phoneHost+"/console/auth/login?"+q.Encode(), phoneHost+"/"); err != nil {
		log.Printf("panel: 手机号登录绑定设备 state 失败（继续尝试换凭证）：%v", err)
	}
}

type phoneToken struct {
	accessToken  string
	refreshToken string
	expiresIn    int64
	domain       string
}

// fetchCredential 第 11 步：换取客户端凭证（这就是最终入池的东西）。
func (s *phoneSession) fetchCredential() (phoneToken, *phoneError) {
	var out phoneToken
	q := url.Values{}
	q.Set("state", s.pluginState)
	_, raw, err := s.do(http.MethodGet, phoneHost+"/v2/plugin/auth/token?"+q.Encode(), nil, "",
		"application/json, text/plain, */*",
		map[string]string{"X-No-Authorization": "true"})
	if err != nil {
		return out, phoneFail("auth_token", "网络错误：%v", err)
	}
	data, perr := phoneEnvelopeData(raw, "auth_token")
	if perr != nil {
		return out, perr
	}
	out.accessToken = phoneNonEmpty(data["accessToken"])
	out.refreshToken = phoneNonEmpty(data["refreshToken"])
	out.domain = phoneNonEmpty(data["domain"])
	if n, ok := data["expiresIn"].(float64); ok {
		out.expiresIn = int64(n)
	}
	if out.accessToken == "" {
		return out, phoneFail("auth_token", "上游未返回 accessToken")
	}
	if out.refreshToken == "" {
		return out, phoneFail("auth_token", "上游未返回 refreshToken（没有刷新令牌的凭证无法续期）")
	}
	if out.domain == "" {
		out.domain = phoneDomain()
	}
	return out, nil
}

// fetchAccount 用刚拿到的 token 拉账号信息（uid/昵称/手机号/企业号）。
// 失败不致命：uid 拿不到时上层会报错，其余字段只是展示用。
func (s *phoneSession) fetchAccount(accessToken, domain string) (map[string]any, error) {
	_, raw, err := s.do(http.MethodGet, phoneHost+"/v2/plugin/accounts", nil, "",
		"application/json, text/plain, */*",
		map[string]string{
			"Authorization":        "Bearer " + accessToken,
			"X-Domain":             orDefault(domain, phoneDomain()),
			"X-No-User-Id":         "true",
			"X-No-Enterprise-Id":   "true",
			"X-No-Department-Info": "true",
		})
	if err != nil {
		return nil, err
	}
	data, perr := phoneEnvelopeData(raw, "accounts")
	if perr != nil {
		return nil, perr
	}
	accounts, _ := data["accounts"].([]any)
	if len(accounts) == 0 {
		return map[string]any{}, nil
	}
	first, _ := accounts[0].(map[string]any)
	if first == nil {
		first = map[string]any{}
	}
	return first, nil
}

// ── HTTP 处理 ─────────────────────────────────────────────────────────

// panelClientIP 客户端 IP（优先 X-Forwarded-For 首个地址，反代后仍能按 IP 节流）。
func panelClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			xff = xff[:i]
		}
		if ip := strings.TrimSpace(xff); ip != "" {
			return ip
		}
	}
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return ip
	}
	return r.RemoteAddr
}

func writePhoneErr(w http.ResponseWriter, status int, e *phoneError) {
	payload := map[string]any{"ok": false, "error": e.message}
	if e.step != "" {
		payload["step"] = e.step
	}
	if e.reason != "" {
		payload["reason"] = e.reason
	}
	if e.retry > 0 {
		payload["retry_after"] = e.retry
		w.Header().Set("Retry-After", fmt.Sprintf("%d", e.retry))
	}
	writeJSON(w, status, payload)
}

// phoneLoginSend 发验证码（sid 为空时先建会话，因此用户只需要点一次）。
//
//	POST /panel/api/login/phone/send  {"sid":"","mobile":"13800138000","country_code":"86"}
func (p *Panel) phoneLoginSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SID         string `json:"sid"`
		Mobile      string `json:"mobile"`
		CountryCode string `json:"country_code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	mobile, country, perr := phoneNormalizeMobile(req.Mobile, req.CountryCode)
	if perr != nil {
		writePhoneErr(w, http.StatusBadRequest, perr)
		return
	}
	if !phoneAcquireGate() {
		writePhoneErr(w, http.StatusServiceUnavailable,
			phoneFail("busy", "当前登录人数较多，请稍后重试"))
		return
	}
	defer phoneReleaseGate()

	var sess *phoneSession
	if strings.TrimSpace(req.SID) != "" {
		got, err := phoneGetSession(req.SID)
		if err != nil {
			writePhoneErr(w, http.StatusBadRequest, err.(*phoneError))
			return
		}
		sess = got
	} else {
		started, err := phoneStartSession()
		if err != nil {
			if pe, ok := err.(*phoneError); ok {
				status := http.StatusBadGateway
				if pe.step == "busy" {
					status = http.StatusServiceUnavailable
				}
				writePhoneErr(w, status, pe)
			} else {
				writeErr(w, http.StatusBadGateway, err.Error())
			}
			return
		}
		sess = started
	}

	// 节流放在「会话已就绪」之后：建会话失败（网络/上游抖动）不该烧掉该号码的
	// 60s 冷却，否则用户重试一次就被堵一分钟，而短信其实一条都没发出去。
	if perr := phoneCheckQuota(panelClientIP(r), country+":"+mobile); perr != nil {
		writePhoneErr(w, http.StatusTooManyRequests, perr)
		return
	}

	expires, err := sess.sendSMS(mobile, country)
	if err != nil {
		pe, ok := err.(*phoneError)
		if !ok {
			pe = phoneFail("sms_send", "%v", err)
		}
		status := http.StatusBadGateway
		if pe.step == "mobile" || pe.step == "session" {
			status = http.StatusBadRequest
		}
		if pe.step == "session" {
			phoneDropSession(sess.sid)
		}
		if pe.retry == 0 && pe.step == "sms_send" {
			pe.retry = int(phoneSMSCooldown.Seconds())
		}
		writePhoneErr(w, status, pe)
		return
	}
	log.Printf("panel: 手机号登录发码完成 sid=%s +%s %s****", sess.sid[:8], country, mobile[:3])
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"sid":         sess.sid,
		"mobile":      mobile,
		"country":     country,
		"expires_in":  expires,
		"retry_after": int(phoneSMSCooldown.Seconds()),
	})
}

// phoneLoginVerify 校验验证码并完成登录（幂等：重复提交复用第一次的成功结果）。
//
//	POST /panel/api/login/phone/verify  {"sid":"...","code":"123456"}
func (p *Panel) phoneLoginVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SID  string `json:"sid"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	code := strings.Map(func(c rune) rune {
		if c >= '0' && c <= '9' {
			return c
		}
		return -1
	}, req.Code)
	if len(code) != 6 {
		writePhoneErr(w, http.StatusBadRequest, phoneFail("code", "请输入 6 位数字验证码"))
		return
	}
	sess, err := phoneGetSession(req.SID)
	if err != nil {
		writePhoneErr(w, http.StatusBadRequest, err.(*phoneError))
		return
	}
	if !phoneAcquireGate() {
		writePhoneErr(w, http.StatusServiceUnavailable,
			phoneFail("busy", "当前登录人数较多，请稍后重试"))
		return
	}
	defer phoneReleaseGate()

	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.result != nil { // 幂等：双击/网络重发的第二个请求直接复用
		writeJSON(w, http.StatusOK, phoneDonePayload(sess.result, p.cfg.Upstream != nil))
		return
	}
	if sess.smsToken == "" {
		writePhoneErr(w, http.StatusBadRequest, phoneFail("code", "请先获取验证码"))
		return
	}

	res, verr := sess.verifyCode(code)
	if verr != nil {
		writePhoneErr(w, http.StatusBadGateway, verr.(*phoneError))
		return
	}

	// 凭证落盘 → 热加载进池（与浏览器登录/JSON 导入同一条收尾路径）。
	duplicated, perr := p.storePhoneCredential(res)
	if perr != nil {
		writePhoneErr(w, http.StatusInternalServerError, perr)
		return
	}
	res.duplicated = duplicated
	sess.consumed = true
	sess.consumedAt = time.Now()
	sess.result = res
	log.Printf("panel: 手机号登录成功 uid=%s nickname=%q duplicated=%v（免重启生效）",
		res.uid, res.nickname, duplicated)
	writeJSON(w, http.StatusOK, phoneDonePayload(res, p.cfg.Upstream != nil))
}

// storePhoneCredential 把换到的凭证写盘并热加载进池。返回是否覆盖了已有账号。
func (p *Panel) storePhoneCredential(res *phoneResult) (bool, *phoneError) {
	tok := res.token
	a := &auth.Auth{
		AccessToken:  tok.accessToken,
		RefreshToken: tok.refreshToken,
		Domain:       tok.domain,
		UID:          res.uid,
		EnterpriseID: res.enterpriseID,
		Nickname:     res.nickname,
		FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", res.uid)),
	}
	if tok.expiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.expiresIn) * time.Second).Unix()
	}
	if err := os.MkdirAll(p.cfg.AuthDir, 0o700); err != nil {
		return false, phoneFail("save", "无法创建账号目录：%v", err)
	}
	// 手机号登录恒为 CN 域（workbuddy.cn）：显式补 realm 键，与存量文件形态统一。
	_, _ = a.BackfillRealm()
	duplicated := p.cfg.Pool != nil && p.cfg.Pool.AuthByUID(res.uid) != nil
	if err := a.SaveAtomic(); err != nil {
		return false, phoneFail("save", "保存凭证失败：%v", err)
	}
	if p.cfg.Pool != nil {
		p.cfg.Pool.Add(a)
		// 全新登录 = 人工恢复口径：清掉旧号遗留的禁用/冷却/熔断。
		p.cfg.Pool.Revive(res.uid)
	}
	p.queueAccountSetup(a)
	return duplicated, nil
}

func phoneDonePayload(res *phoneResult, maintenancePending bool) map[string]any {
	msg := "登录成功，凭证已加入账号池"
	if res.duplicated {
		msg = "该账号此前已存在，已用最新凭证覆盖更新"
	}
	return map[string]any{
		"ok":                  true,
		"uid":                 res.uid,
		"nickname":            res.nickname,
		"realm":               res.realm,
		"duplicated":          res.duplicated,
		"message":             msg,
		"maintenance_pending": maintenancePending,
	}
}

// phoneLoginStatus 会话状态（前端刷新页面后恢复用；也可排查卡在哪一步）。
//
//	GET /panel/api/login/phone/status?sid=...
func (p *Panel) phoneLoginStatus(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	if sid == "" {
		writeJSON(w, http.StatusOK, map[string]any{"alive": false})
		return
	}
	s, err := phoneGetSession(sid)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"alive": false})
		return
	}
	retry := 0
	if !s.codeSentAt.IsZero() {
		if left := s.codeSentAt.Add(phoneSMSCooldown).Sub(time.Now()); left > 0 {
			retry = int(left.Seconds()) + 1
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"alive":        true,
		"mobile":       s.mobile,
		"country_code": s.country,
		"code_sent":    s.smsToken != "",
		"consumed":     s.consumed,
		"retry_after":  retry,
		"age":          int(time.Since(s.created).Seconds()),
	})
}
