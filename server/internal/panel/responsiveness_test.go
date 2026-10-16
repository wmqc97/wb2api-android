package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type responsiveTransport func(*http.Request) (*http.Response, error)

func (f responsiveTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func blockedMaintenancePanel(t *testing.T) (*Panel, <-chan struct{}) {
	t.Helper()
	entered, release := make(chan struct{}, 10), make(chan struct{})
	up := upstream.New()
	up.HTTP.Transport = responsiveTransport(func(r *http.Request) (*http.Response, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return jsonResponse(`{"code":0,"data":{}}`), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	p := New(Config{Pool: pool.New(""), Upstream: up, AuthDir: t.TempDir()})
	t.Cleanup(func() {
		close(release)
		deadline := time.After(5 * time.Second)
		for {
			p.setupMu.Lock()
			workers := p.setupWorkers
			p.setupMu.Unlock()
			if workers == 0 {
				return
			}
			select {
			case <-deadline:
				t.Error("account maintenance workers did not stop")
				return
			case <-time.After(time.Millisecond):
			}
		}
	})
	return p, entered
}

func TestImportReceiptDoesNotWaitForMaintenance(t *testing.T) {
	p, entered := blockedMaintenancePanel(t)
	done := make(chan map[string]any, 1)
	go func() {
		code, result := postImport(t, p, cockpitJSON(cockpitEntry("slow-import", "at", "rt", "www.codebuddy.cn")))
		result["status"] = code
		done <- result
	}()
	select {
	case result := <-done:
		if result["status"] != 200 || result["imported"] != float64(1) || result["maintenance_pending"] != true {
			t.Fatalf("import receipt: %v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("import is waiting for upstream maintenance")
	}
	if p.cfg.Pool.AuthByUID("slow-import") == nil {
		t.Fatal("receipt returned before account entered pool")
	}
	if _, err := os.Stat(filepath.Join(p.cfg.AuthDir, "workbuddy-slow-import.json")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("maintenance was not queued")
	}
}

func TestLoginReceiptDoesNotWaitForMaintenance(t *testing.T) {
	p, entered := blockedMaintenancePanel(t)
	previous := loginHTTP
	loginHTTP = &http.Client{Transport: responsiveTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/auth/token") {
			return jsonResponse(`{"code":0,"data":{"accessToken":"at","refreshToken":"rt","expiresIn":3600,"domain":"www.codebuddy.cn"}}`), nil
		}
		return jsonResponse(`{"code":0,"data":{"uid":"slow-login","nickname":"Example"}}`), nil
	})}
	t.Cleanup(func() { loginHTTP = previous })
	p.logins["ready"] = loginSession{created: time.Now(), realm: "cn"}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/login/poll?state=ready", nil))
		done <- rec
	}()
	select {
	case rec := <-done:
		var result map[string]any
		json.Unmarshal(rec.Body.Bytes(), &result)
		if rec.Code != 200 || result["done"] != true || result["maintenance_pending"] != true {
			t.Fatalf("login receipt: %d %v", rec.Code, result)
		}
	case <-time.After(time.Second):
		t.Fatal("login is waiting for upstream maintenance")
	}
	if p.cfg.Pool.AuthByUID("slow-login") == nil {
		t.Fatal("login did not enter pool")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("login maintenance was not queued")
	}
}

func TestPanelModelsCancelsUpstream(t *testing.T) {
	up := upstream.New()
	up.HTTP.Transport = responsiveTransport(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	p := pool.New("")
	p.Add(&auth.Auth{UID: "slow-models", Domain: "www.codebuddy.cn", AccessToken: "at"})
	pn := New(Config{Pool: p, Upstream: up})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	started := time.Now()
	pn.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/models?realm=cn", nil).WithContext(ctx))
	if time.Since(started) > time.Second || rec.Code != http.StatusBadGateway {
		t.Fatalf("model lookup did not cancel: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLoginPollDoesNotRepeatInFlightRequest(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	previous := loginHTTP
	loginHTTP = &http.Client{Transport: responsiveTransport(func(r *http.Request) (*http.Response, error) {
		close(entered) // A duplicate upstream poll would panic here.
		<-release
		return jsonResponse(`{"code":1,"msg":"pending"}`), nil
	})}
	t.Cleanup(func() { loginHTTP = previous })
	p := New(Config{Pool: pool.New("")})
	p.logins["pending"] = loginSession{created: time.Now(), realm: "cn"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/panel/api/login/poll?state=pending", nil))
	}()
	t.Cleanup(func() { close(release); <-done })
	<-entered
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/login/poll?state=pending", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"done":false`) {
		t.Fatalf("duplicate poll: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPanelAPITimeoutIncludesResponseBody(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync('app.js','utf8');
let mode = 'hang', seen;
const ctx = {AbortController,setTimeout,clearTimeout,LS_KEY:'test',localStorage:{getItem:()=> 'example'},
  openKey:()=>{}, fetch:async (url,opts)=> {seen = opts; return {status:200,ok:true,
    json:()=> mode === 'hang' ? new Promise((resolve,reject)=>opts.signal.addEventListener('abort',()=>reject(new Error('aborted')))) : Promise.resolve({ok:true})};}
};
vm.createContext(ctx);
vm.runInContext(src.slice(src.indexOf('async function api('),src.indexOf('function toast(')),ctx);
(async()=>{
  await assert.rejects(ctx.api('models',{timeoutMs:20}),/请求超时/);
  mode = 'ok'; const form = {};
  await ctx.api('import/cockpit',{body:form,method:'POST',timeoutMs:1000});
  assert.equal(seen.headers.Authorization,'Bearer example');
  assert.equal(seen.headers['Content-Type'],undefined);
  assert.equal(seen.body,form);
})().catch(e=>{console.error(e);process.exit(1);});`
	path := filepath.Join(t.TempDir(), "api-timeout.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("API timeout regression: %v\n%s", err, out)
	}
}

func TestTestChatModelsImmediateFallbackAndRealmIsolation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync('app.js', 'utf8');
const start = src.indexOf('async function loadTestChatModels()');
const end = src.indexOf('// tcLatency', start);
const nodes = {tcModel: {disabled: true, innerHTML: '', value: ''}, tcModelNote: {textContent: ''}};
const pending = [], paths = [];
const ctx = {tcUID: 'cn-account', tcModelSeq: 0, TC_DEFAULT_MODEL: 'deepseek-v4.1-flash',
  overviewData: {accounts: [{uid:'cn-account',realm:'cn'}, {uid:'global-account',realm:'global'}]},
  $: id => nodes[id], esc: s => String(s),
  api: (path, opts) => {paths.push(path); assert.equal(opts.timeoutMs,20000); return new Promise((resolve,reject) => pending.push({resolve,reject}));}
};
vm.createContext(ctx); vm.runInContext(src.slice(start,end),ctx);
(async () => {
  const first = ctx.loadTestChatModels();
  assert.equal(nodes.tcModel.disabled,false);
  assert(nodes.tcModel.innerHTML.includes('deepseek-v4.1-flash'));
  assert.equal(paths[0],'models?realm=cn');
  ctx.tcUID = 'global-account';
  const second = ctx.loadTestChatModels();
  assert.equal(paths[1],'models?realm=global');
  pending[1].resolve({models:[{id:'global-only'}]}); await second;
  pending[0].resolve({models:[{id:'cn-only'}]}); await first;
  assert(nodes.tcModel.innerHTML.includes('global-only'));
  assert(!nodes.tcModel.innerHTML.includes('cn-only'));
  const third = ctx.loadTestChatModels();
  pending[2].reject(new Error('timeout')); await third;
  assert.equal(nodes.tcModel.disabled,false);
  assert(nodes.tcModel.innerHTML.includes('deepseek-v4.1-flash'));
  assert(nodes.tcModelNote.textContent.includes('timeout'));
})().catch(e => {console.error(e);process.exit(1);});`
	path := filepath.Join(t.TempDir(), "models.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("frontend model regression: %v\n%s", err, out)
	}
}
