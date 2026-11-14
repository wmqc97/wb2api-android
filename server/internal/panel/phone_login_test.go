package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// fakePhoneUpstream 假上游：把 11 步链路全部本地化，用于验证「移植后逻辑是否走通」。
//
// 为什么值得写：这条链路的坑全在**状态**上（一次性 state_token、跨域 cookie、
// 第 9 步用 OneID 的 S1 而不是 Keycloak 的 session_code）。只靠真机试错的话，
// 任何一处回归都会表现为「最后一步 11217 login ing」，且看不出是哪步错。
type fakePhoneUpstream struct {
	mu            sync.Mutex
	sendCount     int
	verifyCount   int
	accountsCount int
	tokenCount    int
	// needCaptcha 为 true 时发码返回 captcha（模拟按号风控）。
	needCaptcha bool
}

func (f *fakePhoneUpstream) counts() (send, verify, accounts, token int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sendCount, f.verifyCount, f.accountsCount, f.tokenCount
}

func (f *fakePhoneUpstream) handler(t *testing.T, srvURL string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}

	// 1) 设备授权 state
	mux.HandleFunc("/v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("auth/state 应为 POST，实际 %s", r.Method)
		}
		if got := r.URL.Query().Get("platform"); got != "workbuddy" {
			t.Errorf("platform 应为 workbuddy，实际 %q", got)
		}
		write(w, `{"code":0,"msg":"OK","data":{"state":"PLUGIN-STATE","authUrl":"`+srvURL+`/login"}}`)
	})

	// 2/10) /console/auth/login：无 console 会话 → 302 Keycloak；有会话 → 200（绑定完成）
	mux.HandleFunc("/console/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("KEYCLOAK_SESSION"); err == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("bound"))
			return
		}
		if got := r.URL.Query().Get("state"); got != "PLUGIN-STATE" {
			t.Errorf("console/auth/login state=%q", got)
		}
		if r.Header.Get("X-Domain") == "" {
			t.Error("console/auth/login 缺 X-Domain 头")
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "console", Path: "/"})
		w.Header().Set("Location", "/auth/realms/copilot/protocol/openid-connect/auth?client_id=console&state=kc")
		w.WriteHeader(http.StatusFound)
	})

	// 3) Keycloak 登录页：内嵌 OneID 入口（含 &amp; 实体，验证解转义）
	mux.HandleFunc("/auth/realms/copilot/protocol/openid-connect/auth", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><a class="idp" data-idp="oneid" tabindex="0" href="/auth/realms/copilot/broker/oneid/login?client_id=console&amp;tab_id=t1">OneID</a></html>`))
	})

	// 4) broker/oneid/login → 303 OneID authorize（带 S1）
	mux.HandleFunc("/auth/realms/copilot/broker/oneid/login", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Referer"), "/protocol/openid-connect/auth") {
			t.Errorf("broker 请求 Referer 应指向 Keycloak 登录页，实际 %q", r.Header.Get("Referer"))
		}
		w.Header().Set("Location", srvURL+"/v1/authorize?response_type=code&client_id=ztPxd&state=ONEID-S1")
		w.WriteHeader(http.StatusSeeOther)
	})

	// 5) OneID authorize 预访问
	mux.HandleFunc("/v1/authorize", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>oneid</html>"))
	})

	// 6) 发短信
	mux.HandleFunc("/v1/auth/sms/code/send", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_code"] != "codebuddy" {
			t.Errorf("发码 client_code=%v", body["client_code"])
		}
		if mobile, _ := body["mobile"].(string); !strings.HasPrefix(mobile, "+86 ") || len(mobile) != len("+86 13800138000") {
			t.Errorf("发码 mobile=%v（应为 +86 后跟 11 位号码）", body["mobile"])
		}
		f.mu.Lock()
		f.sendCount++
		captcha := f.needCaptcha
		f.mu.Unlock()
		if captcha {
			write(w, `{"captcha":{"type":"slider"},"status":"need_captcha"}`)
			return
		}
		write(w, `{"captcha":null,"expires_in":60,"status":"unexpired","state_token":"SMS-TOKEN"}`)
	})

	// 7) 校验验证码
	mux.HandleFunc("/v1/auth/sms/code/verify", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["state_token"] != "SMS-TOKEN" {
			t.Errorf("校验 state_token=%v", body["state_token"])
		}
		if body["code"] != "123456" {
			write(w, `{"errCode":1001,"errMessage":"验证码错误"}`)
			return
		}
		f.mu.Lock()
		f.verifyCount++
		f.mu.Unlock()
		write(w, `{"state_token":"SMS-TOKEN"}`)
	})

	// 8) 账号列表 + OneID 授权 code
	mux.HandleFunc("/v1/auth/accounts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state_token") != "SMS-TOKEN" {
			t.Errorf("accounts state_token=%q", r.URL.Query().Get("state_token"))
		}
		f.mu.Lock()
		f.accountsCount++
		f.mu.Unlock()
		write(w, `{"accounts":[{"id":"acc-1"}],"code":"ONEID-CODE"}`)
	})

	// 9) broker endpoint：以 cookie 为成功判据；state 必须是 OneID 的 S1
	mux.HandleFunc("/auth/realms/copilot/broker/oneid/endpoint", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code") != "ONEID-CODE" {
			t.Errorf("broker endpoint code=%q", q.Get("code"))
		}
		if q.Get("state") != "ONEID-S1" {
			t.Errorf("broker endpoint state=%q（应为 OneID 的 S1，不是 Keycloak 的 session_code）", q.Get("state"))
		}
		http.SetCookie(w, &http.Cookie{Name: "KEYCLOAK_SESSION", Value: "kc-session", Path: "/"})
		w.Header().Set("Location", "/console/done")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/console/done", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// 11) 换取凭证
	mux.HandleFunc("/v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-No-Authorization") != "true" {
			t.Error("auth/token 缺 X-No-Authorization: true")
		}
		if r.URL.Query().Get("state") != "PLUGIN-STATE" {
			t.Errorf("auth/token state=%q", r.URL.Query().Get("state"))
		}
		f.mu.Lock()
		f.tokenCount++
		f.mu.Unlock()
		write(w, `{"code":0,"msg":"OK","data":{"accessToken":"at-phone","refreshToken":"rt-phone","expiresIn":3600,"domain":"www.workbuddy.cn"}}`)
	})

	// 账号信息（拿 uid/昵称）
	mux.HandleFunc("/v2/plugin/accounts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-phone" {
			t.Errorf("accounts Authorization=%q", r.Header.Get("Authorization"))
		}
		write(w, `{"code":0,"data":{"accounts":[{"uid":"uid-phone-1","nickname":"手机号号主","phoneNumber":"13800138000"}]}}`)
	})
	return mux
}

// withPhoneUpstream 把 phoneHost/phoneOAuth2 指向假上游，并在测试结束还原。
func withPhoneUpstream(t *testing.T) (*fakePhoneUpstream, string) {
	t.Helper()
	f := &fakePhoneUpstream{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handler(t, srv.URL).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	oldHost, oldOAuth, oldAllowed := phoneHost, phoneOAuth2, phoneAllowedHosts
	phoneHost, phoneOAuth2 = srv.URL, srv.URL
	phoneAllowedHosts = []string{"127.0.0.1", "::1"}
	t.Cleanup(func() {
		phoneHost, phoneOAuth2, phoneAllowedHosts = oldHost, oldOAuth, oldAllowed
	})
	resetPhoneState(t)
	return f, srv.URL
}

// resetPhoneState 清空会话表与发码节流（全局变量，测试间必须隔离）。
func resetPhoneState(t *testing.T) {
	t.Helper()
	phoneMu.Lock()
	for sid, s := range phoneSessions {
		s.cli.CloseIdleConnections()
		delete(phoneSessions, sid)
	}
	phoneMu.Unlock()
	phoneQuota.mu.Lock()
	phoneQuota.byIP = map[string][]time.Time{}
	phoneQuota.byMobile = map[string]time.Time{}
	phoneQuota.exitLast = time.Time{}
	phoneQuota.mu.Unlock()
}

func newPhonePanel(t *testing.T) (*Panel, string) {
	t.Helper()
	authDir := t.TempDir()
	p := New(Config{Version: "test", APIKey: "", Pool: pool.New(""), AuthDir: authDir})
	return p, authDir
}

func postJSON(t *testing.T, pn *Panel, path string, payload map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.RemoteAddr = "10.1.2.3:5555"
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestPhoneNormalizeMobile 号码归一化：剥国家码/去分隔符/按地区校验。
func TestPhoneNormalizeMobile(t *testing.T) {
	cases := []struct {
		mobile, country string
		want            string
		wantErr         bool
	}{
		{"13800138000", "86", "13800138000", false},
		{"+86 138-0013-8000", "86", "13800138000", false},
		{"8613800138000", "86", "13800138000", false},
		{"138 0013 8000", "", "13800138000", false}, // 缺省 86
		{"51234567", "852", "51234567", false},
		{"61234567", "853", "61234567", false},
		{"1380013800", "86", "", true},  // 10 位
		{"23800138000", "86", "", true}, // 第二位非 3-9
		{"13800138000", "1", "", true},  // 不支持的地区
	}
	for _, c := range cases {
		got, country, err := phoneNormalizeMobile(c.mobile, c.country)
		if c.wantErr {
			if err == nil {
				t.Errorf("normalize(%q,%q) 应报错，实际得到 %q", c.mobile, c.country, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalize(%q,%q) 意外报错：%v", c.mobile, c.country, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalize(%q,%q)=%q want %q", c.mobile, c.country, got, c.want)
		}
		if c.country == "" && country != "86" {
			t.Errorf("缺省地区应为 86，实际 %q", country)
		}
	}
}

// TestPhoneLoginEndToEnd 全链路：发码 → 校验 → 换凭证 → 落盘 → 进池（不跳转官方页）。
func TestPhoneLoginEndToEnd(t *testing.T) {
	f, _ := withPhoneUpstream(t)
	pn, authDir := newPhonePanel(t)

	status, body := postJSON(t, pn, "/panel/api/login/phone/send",
		map[string]any{"mobile": "13800138000", "country_code": "86"})
	if status != http.StatusOK {
		t.Fatalf("send status=%d body=%v", status, body)
	}
	sid, _ := body["sid"].(string)
	if sid == "" {
		t.Fatal("send 未返回 sid（后续校验无法继续）")
	}
	if body["retry_after"].(float64) != 60 {
		t.Errorf("retry_after=%v（应为 60）", body["retry_after"])
	}

	// 会话状态可查（前端刷新后恢复）
	req := httptest.NewRequest(http.MethodGet, "/panel/api/login/phone/status?sid="+sid, nil)
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	var st map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st["alive"] != true || st["code_sent"] != true {
		t.Errorf("status=%v（应为 alive+code_sent）", st)
	}

	status, body = postJSON(t, pn, "/panel/api/login/phone/verify",
		map[string]any{"sid": sid, "code": "123456"})
	if status != http.StatusOK {
		t.Fatalf("verify status=%d body=%v", status, body)
	}
	if body["uid"] != "uid-phone-1" {
		t.Errorf("uid=%v", body["uid"])
	}
	if body["nickname"] != "手机号号主" {
		t.Errorf("nickname=%v", body["nickname"])
	}
	if body["duplicated"] != false {
		t.Errorf("首次登录不应判重复：%v", body["duplicated"])
	}

	// 凭证落盘 + 进池
	file := filepath.Join(authDir, "workbuddy-uid-phone-1.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("凭证未落盘：%v", err)
	}
	if !strings.Contains(string(raw), "at-phone") || !strings.Contains(string(raw), "rt-phone") {
		t.Errorf("落盘内容缺 token：%s", raw)
	}
	if pn.cfg.Pool.AuthByUID("uid-phone-1") == nil {
		t.Error("账号未热加载进池")
	}

	// 幂等：重复提交复用第一次结果，不再打上游
	_, verifyBefore, accountsBefore, tokenBefore := f.counts()
	status, body = postJSON(t, pn, "/panel/api/login/phone/verify",
		map[string]any{"sid": sid, "code": "123456"})
	if status != http.StatusOK {
		t.Fatalf("重复 verify status=%d body=%v", status, body)
	}
	_, verifyAfter, accountsAfter, tokenAfter := f.counts()
	if verifyAfter != verifyBefore || accountsAfter != accountsBefore || tokenAfter != tokenBefore {
		t.Errorf("重复提交又打了一次上游：verify %d→%d accounts %d→%d token %d→%d",
			verifyBefore, verifyAfter, accountsBefore, accountsAfter, tokenBefore, tokenAfter)
	}

	// 同一号码 60s 内二次发码被节流（带 retry_after）
	status, body = postJSON(t, pn, "/panel/api/login/phone/send",
		map[string]any{"mobile": "13800138000", "country_code": "86"})
	if status != http.StatusTooManyRequests {
		t.Fatalf("二次发码应 429，实际 %d body=%v", status, body)
	}
	if _, ok := body["retry_after"]; !ok {
		t.Error("节流响应缺 retry_after（前端无法倒计时）")
	}
}

// TestPhoneLoginBadMobile 号码非法 → 400 + step=mobile（前端据此标红输入框）。
func TestPhoneLoginBadMobile(t *testing.T) {
	withPhoneUpstream(t)
	pn, _ := newPhonePanel(t)
	status, body := postJSON(t, pn, "/panel/api/login/phone/send",
		map[string]any{"mobile": "12345", "country_code": "86"})
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if body["step"] != "mobile" {
		t.Errorf("step=%v（应为 mobile）", body["step"])
	}
}

// TestPhoneLoginNeedCaptcha 上游按号风控要图形验证码时，如实报 need_captcha 而不是死循环重试。
func TestPhoneLoginNeedCaptcha(t *testing.T) {
	f, _ := withPhoneUpstream(t)
	f.needCaptcha = true
	pn, _ := newPhonePanel(t)
	status, body := postJSON(t, pn, "/panel/api/login/phone/send",
		map[string]any{"mobile": "13900139000", "country_code": "86"})
	if status != http.StatusBadGateway {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if body["reason"] != "need_captcha" {
		t.Errorf("reason=%v（应为 need_captcha）", body["reason"])
	}
	if !strings.Contains(body["error"].(string), "安全验证") {
		t.Errorf("错误文案应说明需要安全验证：%v", body["error"])
	}
}

// TestPhoneLoginFrontendWiring 前端接线：Add Account 对话框必须有「手机号登录」标签页、
// 号码/验证码输入、发码与登录按钮，且调用的是本文件的三个端点（缺一处用户就点不到）。
func TestPhoneLoginFrontendWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, must := range []string{
		"login/phone/send",
		"login/phone/verify",
		"'addTabPhone'",
		"'phoneMobile'",
		"'phoneCode'",
		"'btnPhoneSend'",
		"'btnPhoneVerify'",
		"switchAddTab(",
	} {
		if !strings.Contains(s, must) {
			t.Errorf("app.js 缺手机号登录接线：%s", must)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, must := range []string{
		"id=\"addTabPhone\"",
		"id=\"phoneMobile\"",
		"id=\"phoneCode\"",
		"id=\"btnPhoneSend\"",
		"id=\"btnPhoneVerify\"",
		"id=\"phoneCountry\"",
		"data-tab=\"phone\"",
	} {
		if !strings.Contains(h, must) {
			t.Errorf("index.html 缺手机号登录 DOM：%s", must)
		}
	}
}
