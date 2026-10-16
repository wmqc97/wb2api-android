package com.wb2api.gateway;

import android.Manifest;
import android.app.Activity;
import android.app.AlertDialog;
import android.content.ActivityNotFoundException;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.content.res.Configuration;
import android.graphics.Typeface;
import android.graphics.drawable.ClipDrawable;
import android.graphics.drawable.GradientDrawable;
import android.graphics.drawable.LayerDrawable;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.provider.Settings;
import android.text.InputType;
import android.text.TextUtils;
import android.util.Log;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.ViewTreeObserver;
import android.view.Window;
import android.view.WindowManager;
import android.webkit.CookieManager;
import android.webkit.DownloadListener;
import android.webkit.JavascriptInterface;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.EditText;
import android.widget.ImageButton;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.PopupMenu;
import android.widget.ProgressBar;
import android.widget.ScrollView;
import android.widget.Switch;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONObject;
import org.json.JSONArray;

import java.util.ArrayList;
import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Locale;

/**
 * 主界面 = 内嵌 WebView 承载服务端自带的控制台。
 *
 * 面板本身还是那个面板（index.html/app.js），App 只做三件原生的事：
 *   1. 保证网关进程在跑（交给 {@link GatewayService}）；
 *   2. 就绪后打开 http://127.0.0.1:PORT/panel/；
 *   3. 把 config.json 里的 api_key 预注入 localStorage，免去每次手填密钥。
 */
public class MainActivity extends Activity implements GatewayState.Listener {

    private static final String TAG = "Wb2ApiMain";

    private static final int REQ_FILE = 1001;
    private static final int REQ_NOTIFICATION = 1002;
    private static final int REQ_EXPORT = 1003;
    private static final int MAX_EXPORT_BYTES = 32 * 1024 * 1024;
    private static final int MAX_RELEASE_BODY_CHARS = 1200;

    private static final String UPSTREAM_URL = "https://github.com/CSSZYF/wb2api";
    private static final String LATEST_RELEASE_API =
            "https://api.github.com/repos/CSSZYF/wb2api/releases/latest";

    /** 面板读取密钥用的 localStorage 键，必须与 app.js 的 LS_KEY 一致。 */
    private static final String LS_KEY = "wb2api.key";

    private WebView web;
    private View overlay;
    private ProgressBar spinner;
    private TextView overlayText;
    private Button retryButton;
    private TextView statusText;
    private ImageButton menuButton;

    // 首页
    private LinearLayout homeScreen;
    private LinearLayout panelScreen;
    private TextView homeStatusText;
    private ImageButton powerButton;
    private TextView localUrlText;
    private TextView homeVersionText;
    private TextView wifiUrlText;
    private TextView apiKeyText;
    private EditText portInput;

    private ValueCallback<Uri[]> fileCallback;

    private final Object exportLock = new Object();
    private String pendingExportName;
    private String pendingExportContent;

    /** 本次运行是否已经把 api_key 写进 localStorage（避免每次加载都重放）。 */
    private boolean seeded;
    /** 是否已通过健康检查。 */
    private boolean ready;
    /** 防止用户连续点击时重复请求 GitHub API。 */
    private boolean updateCheckRunning;
    /** 防止批量菜单操作重复提交。 */
    private boolean panelActionRunning;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        web = findViewById(R.id.web);
        overlay = findViewById(R.id.overlay);
        spinner = findViewById(R.id.spinner);
        overlayText = findViewById(R.id.overlayText);
        retryButton = findViewById(R.id.retryButton);
        statusText = findViewById(R.id.statusText);
        menuButton = findViewById(R.id.menuButton);

        homeScreen = findViewById(R.id.homeScreen);
        panelScreen = findViewById(R.id.panelScreen);
        homeStatusText = findViewById(R.id.homeStatusText);
        powerButton = findViewById(R.id.powerButton);
        localUrlText = findViewById(R.id.localUrlText);
        homeVersionText = findViewById(R.id.homeVersion);
        wifiUrlText = findViewById(R.id.wifiUrlText);
        apiKeyText = findViewById(R.id.apiKeyText);
        portInput = findViewById(R.id.portInput);

        applyWindowInsets(findViewById(R.id.root));
        setupWebView();

        menuButton.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                showMenu();
            }
        });
        retryButton.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                seeded = false;
                clearWebCache();
                GatewayService.restart(MainActivity.this);
            }
        });

        findViewById(R.id.modelTierButton).setOnClickListener(v -> showModelsDialog());

        View browserButton = findViewById(R.id.browserButton);
        browserButton.setOnClickListener(v -> openPanelInBrowser());
        // 单击用系统浏览器打开（局域网地址，方便同 Wi-Fi 的电脑/平板访问）；
        // 长按切到应用内嵌的 WebView 控制台——两条路径都常用，但不值得占两个图标位。
        browserButton.setOnLongClickListener(v -> {
            showPanel();
            return true;
        });
        findViewById(R.id.settingsButton).setOnClickListener(v -> showMenu());
        findViewById(R.id.backButton).setOnClickListener(v -> showHome());
        powerButton.setOnClickListener(v -> togglePower());
        findViewById(R.id.copyLocalButton).setOnClickListener(
                v -> copy(getString(R.string.home_copy_url), localUrl()));
        findViewById(R.id.copyWifiButton).setOnClickListener(
                v -> copy(getString(R.string.home_copy_url), consoleUrl()));
        findViewById(R.id.copyApiKeyButton).setOnClickListener(v -> copyApiKey());
        findViewById(R.id.editApiKeyButton).setOnClickListener(v -> editApiKey());
        findViewById(R.id.resetPortButton).setOnClickListener(
                v -> portInput.setText(String.valueOf(Prefs.port(this))));
        findViewById(R.id.savePortButton).setOnClickListener(v -> savePort());
        findViewById(R.id.btnCheckinAll).setOnClickListener(v -> confirmCheckinAll());
        findViewById(R.id.btnRunAllTasks).setOnClickListener(v -> confirmRunAllTasks());
        findViewById(R.id.btnShowLogs).setOnClickListener(v -> showLogs());
        findViewById(R.id.btnShowCredits).setOnClickListener(v -> showCreditsDialog());

        refreshHomeInfo();

        GatewayState.addListener(this);
        // 先要通知权限、再启动网关服务：前台服务的常驻通知依赖 POST_NOTIFICATIONS，
        // Android 13+ 上没有这个权限时 startForeground 的通知会被系统静默丢弃，
        // 于是"服务在跑但状态栏什么都没有"，看起来就像通知没出现过。
        requestNotificationPermission();
        GatewayService.start(this);
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        if (ready && web != null && web.getUrl() == null) {
            loadPanel();
        }
    }

    @Override
    protected void onDestroy() {
        GatewayState.removeListener(this);
        if (web != null) {
            web.destroy();
            web = null;
        }
        super.onDestroy();
    }

    @Override
    public void onBackPressed() {
        // 控制台屏幕：先退 WebView 历史，退不动就回首页；在首页上再按一次才退出 App
        //（网关继续在后台跑）。
        if (panelVisible()) {
            if (web != null && web.canGoBack()) {
                web.goBack();
                return;
            }
            showHome();
            return;
        }
        super.onBackPressed();
    }

    // ── 首页 ──────────────────────────────────────────────────────────────

    /** 控制台屏幕是否在前台。 */
    private boolean panelVisible() {
        return panelScreen != null && panelScreen.getVisibility() == View.VISIBLE;
    }

    /** 切到控制台（WebView）屏幕。 */
    private void showPanel() {
        if (panelScreen == null) {
            return;
        }
        homeScreen.setVisibility(View.GONE);
        panelScreen.setVisibility(View.VISIBLE);
        if (ready && web != null && web.getUrl() == null) {
            loadPanel();
            return;
        }
        // 网关还没就绪：WebView 是空的，必须把遮罩也一起摆出来，否则切过去只看到一片白。
        // 遮罩文案取当前状态（正在启动 / 启动失败 / 已停止），就绪后 onStateChanged 会摘掉它。
        if (!ready) {
            GatewayState.Status status = GatewayState.status();
            if (status == GatewayState.Status.ERROR) {
                showOverlay(GatewayState.message(), true);
            } else if (status == GatewayState.Status.STOPPED) {
                showOverlay("网关服务已停止", true);
            } else {
                String msg = GatewayState.message();
                showOverlay(msg == null || msg.isEmpty() ? getString(R.string.loading) : msg, false);
            }
        }
    }

    /** 切回首页。 */
    private void showHome() {
        if (homeScreen == null) {
            return;
        }
        panelScreen.setVisibility(View.GONE);
        homeScreen.setVisibility(View.VISIBLE);
        refreshHomeInfo();
    }

    /** 本机回环控制台地址。 */
    private String localUrl() {
        return ServerProcess.baseUrl(this);
    }

    /** 局域网控制台地址；未开启局域网或没拿到 IP 时返回 null。 */
    private String wifiUrl() {
        if (!Prefs.lan(this)) {
            return null;
        }
        String ip = LocalIp.get();
        return (ip == null || ip.isEmpty()) ? null : "http://" + ip + ":" + Prefs.port(this);
    }

    /**
     * 对外使用的控制台地址：局域网优先。
     *
     * 浏览器打开与「复制控制台地址」都走这里，这样同一 Wi-Fi 下的电脑/平板也能直接
     * 打开面板；没开局域网时回落到 127.0.0.1。
     */
    private String consoleUrl() {
        String wifi = wifiUrl();
        return wifi != null ? wifi : localUrl();
    }

    /** 刷新首页上的控制台地址、密钥与端口输入框。 */
    private void refreshHomeInfo() {
        if (localUrlText == null) {
            return;
        }
        // 版本号取自 APK 的 versionName（与「关于」页、更新检查同一来源），
        // 带 v 前缀和网关侧的 appVersion 保持一致。
        if (homeVersionText != null && homeVersionText.length() == 0) {
            homeVersionText.setText("v" + appVersionName());
        }
        localUrlText.setText(localUrl());

        String wifi = wifiUrl();
        wifiUrlText.setText(wifi != null ? wifi : getString(R.string.home_offline));
        wifiUrlText.setTextColor(getColor(wifi != null ? R.color.ink : R.color.ink3));

        String key = ServerProcess.readApiKey(this);
        apiKeyText.setText(key.isEmpty() ? getString(R.string.home_api_key_none) : key);

        if (portInput != null && !portInput.hasFocus()) {
            portInput.setText(String.valueOf(Prefs.port(this)));
        }
    }

    /** 首页状态卡：文案与启停按钮外观随网关状态变化。 */
    private void updateHomeStatus(GatewayState.Status status) {
        if (homeStatusText == null) {
            return;
        }
        int label;
        switch (status) {
            case READY:
                label = R.string.status_running;
                break;
            case STARTING:
                label = R.string.status_starting;
                break;
            case ERROR:
                label = R.string.status_error;
                break;
            default:
                label = R.string.status_stopped;
                break;
        }
        homeStatusText.setText(label);

        boolean up = status == GatewayState.Status.READY || status == GatewayState.Status.STARTING;
        powerButton.setBackgroundResource(up ? R.drawable.bg_circle_red : R.drawable.bg_circle_green);
        powerButton.setImageResource(up ? R.drawable.ic_stop : R.drawable.ic_play);
        powerButton.setContentDescription(
                getString(up ? R.string.action_stop_gateway : R.string.action_start_gateway));

        refreshHomeInfo();
    }

    /** 首页大按钮：运行中/启动中 → 停止；已停止/失败 → 启动。 */
    private void togglePower() {
        GatewayState.Status status = GatewayState.status();
        if (status == GatewayState.Status.READY || status == GatewayState.Status.STARTING) {
            GatewayService.stop(this);
            toast("正在停止网关服务…");
        } else {
            seeded = false;
            clearWebCache();
            GatewayService.start(this);
            toast("正在启动网关服务…");
        }
    }

    /** 首页「网关配置」里的保存：写端口并重启网关。 */
    private void savePort() {
        int port;
        try {
            port = Integer.parseInt(portInput.getText().toString().trim());
        } catch (Exception e) {
            toast("端口不合法");
            return;
        }
        if (port < 1024 || port > 65535) {
            toast("端口需在 1024 - 65535 之间");
            return;
        }
        if (port == Prefs.port(this)) {
            toast("端口未变化");
            return;
        }
        Prefs.setPort(this, port);
        seeded = false;
        clearWebCache();
        GatewayService.restart(this);
        toast("端口已保存，正在重启网关…");
    }

    // ── WebView 装配 ───────────────────────────────────────────────────────

    /**
     * targetSdk 35 起 Android 15 强制边到边，状态栏/手势条会压在内容上，
     * 而系统主题里的 statusBarColor 已失效。这里按系统栏 inset 给根布局留白，
     * 旧版本 DecorFitsSystemWindows 已消费掉 inset（返回 0），不会重复留白。
     */
    private void applyWindowInsets(final View root) {
        final int left = root.getPaddingLeft();
        final int top = root.getPaddingTop();
        final int right = root.getPaddingRight();
        final int bottom = root.getPaddingBottom();
        root.setOnApplyWindowInsetsListener(new View.OnApplyWindowInsetsListener() {
            @Override
            public android.view.WindowInsets onApplyWindowInsets(View v, android.view.WindowInsets insets) {
                int barTop;
                int barBottom;
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                    android.graphics.Insets bars = insets.getInsets(android.view.WindowInsets.Type.systemBars());
                    barTop = bars.top;
                    barBottom = bars.bottom;
                } else {
                    barTop = insets.getSystemWindowInsetTop();
                    barBottom = insets.getSystemWindowInsetBottom();
                }
                v.setPadding(left, top + barTop, right, bottom + barBottom);
                return insets;
            }
        });
        root.requestApplyInsets();
    }

    @SuppressWarnings("SetJavaScriptEnabled")
    private void setupWebView() {
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setDatabaseEnabled(true);
        s.setLoadWithOverviewMode(true);
        s.setUseWideViewPort(true);
        // 面板是桌面版控制台，手机宽度下有些区块会被裁，留出双指缩放但不要那对 +/- 按钮。
        s.setBuiltInZoomControls(true);
        s.setDisplayZoomControls(false);
        s.setSupportMultipleWindows(false);
        s.setJavaScriptCanOpenWindowsAutomatically(true);
        // 面板与 API 同源（都是 http://127.0.0.1:PORT），但上游偶发 https 跳转时别被拦。
        s.setMixedContentMode(WebSettings.MIXED_CONTENT_ALWAYS_ALLOW);
        s.setAllowFileAccess(false);
        // 必须允许：面板的“导入账号”走 <input type=file>，选中的是 content:// URI。
        s.setAllowContentAccess(true);
        s.setCacheMode(WebSettings.LOAD_DEFAULT);

        // 面板导出账号时把 JSON 交给原生另存为；仅本地面板可通过方法内的 URL 校验。
        web.addJavascriptInterface(new ExportBridge(), "Wb2Api");
        web.setBackgroundColor(getColor(R.color.bg));
        CookieManager.getInstance().setAcceptCookie(true);
        web.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
                return handleUrl(request.getUrl());
            }

            @Override
            @SuppressWarnings("deprecation")
            public boolean shouldOverrideUrlLoading(WebView view, String url) {
                return handleUrl(Uri.parse(url));
            }

            @Override
            public void onPageFinished(WebView view, String url) {
                if (url == null || !url.startsWith(ServerProcess.baseUrl(MainActivity.this))) {
                    return;
                }
                seedApiKey();
                hideOverlay();
            }
        });
        web.setWebChromeClient(new WebChromeClient() {
            @Override
            public boolean onShowFileChooser(WebView view, ValueCallback<Uri[]> callback,
                                             FileChooserParams params) {
                if (fileCallback != null) {
                    fileCallback.onReceiveValue(null);
                }
                fileCallback = callback;
                Intent pick = new Intent(Intent.ACTION_GET_CONTENT);
                pick.addCategory(Intent.CATEGORY_OPENABLE);
                pick.setType("*/*");
                pick.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true);
                try {
                    startActivityForResult(Intent.createChooser(pick, "选择文件"), REQ_FILE);
                    return true;
                } catch (ActivityNotFoundException e) {
                    fileCallback = null;
                    return false;
                }
            }
        });
        web.setDownloadListener(new DownloadListener() {
            @Override
            public void onDownloadStart(String url, String userAgent, String disposition,
                                        String mime, long size) {
                // 面板导出多为 blob:/data:，WebView 接不住，统一交给系统浏览器下载。
                if (url == null || url.startsWith("blob:") || url.startsWith("data:")) {
                    toast("该导出由面板直接生成，请在浏览器中打开面板完成下载");
                    return;
                }
                openExternal(Uri.parse(url));
            }
        });
    }

    private boolean handleUrl(Uri uri) {
        if (uri == null) {
            return false;
        }
        String scheme = uri.getScheme() == null ? "" : uri.getScheme().toLowerCase();
        if ("http".equals(scheme) || "https".equals(scheme)) {
            if (uri.toString().startsWith(ServerProcess.baseUrl(this))) {
                return false; // 本地面板/接口：留在 WebView
            }
            openExternal(uri);
            return true;
        }
        if ("about".equals(scheme) || "javascript".equals(scheme)
                || "data".equals(scheme) || "blob".equals(scheme)) {
            return false;
        }
        openExternal(uri);
        return true;
    }

    private void openExternal(Uri uri) {
        try {
            startActivity(new Intent(Intent.ACTION_VIEW, uri)
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK));
        } catch (ActivityNotFoundException e) {
            toast("没有可打开该链接的应用");
        }
    }

    /** 面板 API 全靠 Authorization: Bearer <key>，把 config.json 里的密钥预置进 localStorage。 */
    private void seedApiKey() {
        if (seeded || web == null) {
            return;
        }
        String key = ServerProcess.readApiKey(this);
        if (key.isEmpty()) {
            return; // 首启 config.json 还没落盘，下一次 onPageFinished 再试
        }
        seeded = true;
        String js = "(function(){try{var want=" + JSONObject.quote(key)
                + ";if(localStorage.getItem('" + LS_KEY + "')!==want){localStorage.setItem('"
                + LS_KEY + "',want);location.reload();}}catch(e){}})()";
        web.evaluateJavascript(js, null);
    }

    private void loadPanel() {
        if (web == null) {
            return;
        }
        seeded = false;
        web.loadUrl(ServerProcess.panelUrl(this));
    }

    private void clearWebCache() {
        if (web != null) {
            web.clearCache(true);
        }
    }

    // ── 状态回调 ──────────────────────────────────────────────────────────

    @Override
    public void onStateChanged(final GatewayState.Status status, final String message) {
        if (statusText != null) {
            statusText.setText(message);
            statusText.setTextColor(getColor(colorOf(status)));
        }
        updateHomeStatus(status);
        switch (status) {
            case READY: {
                boolean first = !ready;
                ready = true;
                if (first) {
                    loadPanel();
                }
                hideOverlay();
                break;
            }
            case ERROR:
                ready = false;
                showOverlay(message, true);
                break;
            case STOPPED:
                ready = false;
                showOverlay("网关服务已停止", true);
                break;
            case STARTING:
            default:
                ready = false;
                showOverlay(message, false);
                break;
        }
    }

    private int colorOf(GatewayState.Status status) {
        switch (status) {
            case READY:
                return R.color.ok;
            case ERROR:
                return R.color.bad;
            case STARTING:
                return R.color.warn;
            default:
                return R.color.ink3;
        }
    }

    private void showOverlay(String text, boolean showRetry) {
        if (overlay == null) {
            return;
        }
        overlay.setVisibility(View.VISIBLE);
        spinner.setVisibility(showRetry ? View.GONE : View.VISIBLE);
        overlayText.setText(text == null || text.isEmpty() ? getString(R.string.loading) : text);
        retryButton.setVisibility(showRetry ? View.VISIBLE : View.GONE);
    }

    private void hideOverlay() {
        if (overlay != null) {
            overlay.setVisibility(View.GONE);
        }
    }

    // ── 菜单 ──────────────────────────────────────────────────────────────

    private void showMenu() {
        // 首页上「更多」锚在右上角齿轮，控制台里锚在原来的竖排菜单按钮上。
        View anchor = panelVisible() ? menuButton : findViewById(R.id.settingsButton);
        PopupMenu menu = new PopupMenu(this, anchor);
        // 签到 / 任务闭环 / 日志 / 积分明细都已挪到首页快捷操作行；
        // 「刷新面板」「浏览器打开面板」「复制 API 密钥」「启停网关」不再放菜单里——
        // 前两项目前只对控制台屏幕有意义，密钥看首页卡片，启停首页有圆形按钮。
        // 这里的「更多」只保留仍需菜单承载的系统类操作。
        menu.getMenu().add(0, 4, 0, R.string.action_copy);
        menu.getMenu().add(0, 6, 1, R.string.action_settings);
        menu.getMenu().add(0, 9, 2, R.string.action_about);
        menu.setOnMenuItemClickListener(new PopupMenu.OnMenuItemClickListener() {
            @Override
            public boolean onMenuItemClick(android.view.MenuItem item) {
                switch (item.getItemId()) {
                    case 4:
                        copyAccessInfo();
                        return true;
                    case 6:
                        showSettings();
                        return true;
                    case 9:
                        showAbout();
                        return true;
                    default:
                        return false;
                }
            }
        });
        menu.show();
    }

    private void confirmCheckinAll() {
        if (!ensurePanelActionReady()) {
            return;
        }
        showDialog(dialogBuilder()
                .setTitle(R.string.action_checkin_all)
                .setMessage("将触发全部启用账号签到，并顺带执行猫猫旅行巡检。\n执行进度可在运行日志中查看。")
                .setNegativeButton("取消", null)
                .setPositiveButton("开始", (dialog, which) -> postPanelAction(
                        "/panel/api/checkin_all",
                        "",
                        "全部签到已触发，结果见运行日志"))
        );
    }

    private void confirmRunAllTasks() {
        if (!ensurePanelActionReady()) {
            return;
        }
        // 这一键就是「添加所有账号执行闭环，然后执行所有待办」：
        // run_queue 会先扫全部账号的待办、把未接受的任务批量接受掉（上游对
        // not_accepted 的任务不计数，必须先 accept 才落账），再按依赖顺序逐个
        // 执行并领取奖励，账号内串行、默认并发 1。
        showDialog(dialogBuilder()
                .setTitle(R.string.action_run_all_tasks)
                .setMessage("将把全部账号的待办任务加入执行闭环：先批量接受未接受的任务，"
                        + "再逐个执行并自动领取奖励。\n"
                        + "账号内串行、默认并发 1；含真实对话的任务耗时较长。\n"
                        + "执行进度可在任务中心与运行日志中查看。")
                .setNegativeButton("取消", null)
                .setPositiveButton("开始", (dialog, which) -> postPanelAction(
                        "/panel/api/tasks/run_queue",
                        "{\"concurrency\":1,\"growth\":true,\"school\":true}",
                        "任务队列已启动"))
        );
    }

    private boolean ensurePanelActionReady() {
        if (!ready) {
            toast("网关尚未就绪，请稍后再试");
            return false;
        }
        // 不校验密钥：默认不开启鉴权时密钥为空，面板接口同样放行。
        if (panelActionRunning) {
            toast("已有批量请求正在提交");
            return false;
        }
        return true;
    }

    /**
     * 面板批量动作都要求 Bearer 密钥；扫描全部待办会同步访问上游，因此在后台线程
     * 提交并把响应带回主线程提示。
     */
    private void postPanelAction(final String path, final String json, final String successText) {
        if (panelActionRunning) {
            toast("已有批量请求正在提交");
            return;
        }
        final String key = ServerProcess.readApiKey(this);
        panelActionRunning = true;

        new Thread(new Runnable() {
            @Override
            public void run() {
                HttpURLConnection connection = null;
                String error = null;
                String message = successText;
                try {
                    URL url = new URL(ServerProcess.baseUrl(MainActivity.this) + path);
                    connection = (HttpURLConnection) url.openConnection();
                    connection.setConnectTimeout(8000);
                    connection.setReadTimeout(120000);
                    connection.setRequestMethod("POST");
                    connection.setDoOutput(true);
                    if (!key.isEmpty()) {
                        connection.setRequestProperty("Authorization", "Bearer " + key);
                    }
                    connection.setRequestProperty("Content-Type", "application/json");
                    if (json != null && !json.isEmpty()) {
                        try (OutputStream out = connection.getOutputStream()) {
                            out.write(json.getBytes(StandardCharsets.UTF_8));
                        }
                    }

                    int status = connection.getResponseCode();
                    InputStream stream = status >= 200 && status < 300
                            ? connection.getInputStream()
                            : connection.getErrorStream();
                    String body = readText(stream);
                    if (status < 200 || status >= 300) {
                        error = panelError(status, body);
                    } else {
                        JSONObject result = new JSONObject(body);
                        if (!result.optBoolean("started", false)) {
                            message = result.optString("message", "没有可执行的任务");
                        } else if (result.has("total")) {
                            message = "任务队列已启动：" + result.optInt("total") + " 项";
                        }
                    }
                } catch (Exception e) {
                    error = e.getMessage() == null || e.getMessage().isEmpty()
                            ? "请求失败"
                            : e.getMessage();
                    Log.w(TAG, "批量操作请求失败", e);
                } finally {
                    if (connection != null) {
                        connection.disconnect();
                    }
                }

                final String failure = error;
                final String text = message;
                runOnUiThread(new Runnable() {
                    @Override
                    public void run() {
                        panelActionRunning = false;
                        if (isFinishing() || isDestroyed()) {
                            return;
                        }
                        toast(failure == null ? text : "操作失败：" + failure);
                    }
                });
            }
        }, "wb2api-panel-action").start();
    }

    private String panelError(int status, String body) {
        if (status == 401) {
            return "API 密钥无效";
        }
        try {
            String message = new JSONObject(body).optString("error", "");
            if (!message.isEmpty()) {
                return message;
            }
        } catch (Exception ignored) {
            // 非 JSON 错误体走通用 HTTP 文案。
        }
        return "HTTP " + status;
    }

    private void copyApiKey() {
        String key = ServerProcess.readApiKey(this);
        if (key.isEmpty()) {
            toast("未设置 API Key（当前不鉴权）");
            return;
        }
        copy("wb2api API Key", key);
    }

    /**
     * 修改 / 关闭 API Key。
     *
     * 输入框留空 = 完全关闭鉴权（api_key 写成空串，httpauth 对空 key 恒放行），
     * 这是本 fork 的默认姿态，也是用户明确要的"完全关闭 key"入口。
     * api_key 由服务端启动时读入内存，所以保存后必须重启网关才生效。
     */
    private void editApiKey() {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        int pad = dp(20);
        box.setPadding(pad, dp(8), pad, 0);

        final EditText input = new EditText(this);
        input.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_VISIBLE_PASSWORD);
        input.setHint(R.string.edit_key_hint);
        input.setText(ServerProcess.readApiKey(this));
        input.setSelection(input.getText().length());
        box.addView(input);

        TextView note = new TextView(this);
        note.setText(R.string.edit_key_note);
        note.setTextColor(getColor(R.color.ink3));
        note.setTextSize(12);
        note.setPadding(dp(2), dp(8), 0, 0);
        box.addView(note);

        showDialog(dialogBuilder()
                .setTitle(R.string.edit_key_title)
                .setView(box)
                .setPositiveButton("保存并重启网关", (d, w) -> saveApiKey(input.getText().toString()))
                .setNeutralButton(R.string.edit_key_clear, (d, w) -> saveApiKey(""))
                .setNegativeButton("取消", null)
        );
    }

    private void saveApiKey(String key) {
        try {
            ServerProcess.writeApiKey(this, key);
        } catch (Exception e) {
            toast("保存失败：" + e.getMessage());
            return;
        }
        // 首页立刻反映新值，不用等重启完成。
        refreshHomeInfo();
        clearWebCache();
        GatewayService.restart(this);
        toast(key.trim().isEmpty() ? "已关闭鉴权，正在重启网关" : "密钥已保存，正在重启网关");
    }

    private void openPanelInBrowser() {
        String key = ServerProcess.readApiKey(this);
        // 设了密钥才复制：浏览器里打开面板要手填一次密钥，先替用户放进剪贴板。
        // 没设密钥时**既不复制也不提示**——默认不鉴权，复制空串只会污染剪贴板，
        // 弹个"未设置密钥"更是噪音（面板本来就直接打得开）。
        if (!key.isEmpty()) {
            copyKeyForBrowser(key);
        }
        // 用局域网地址打开：同一 Wi-Fi 下的电脑/平板也能直接进控制台。
        openExternal(Uri.parse(consoleUrl() + "/panel/"));
    }

    /** 浏览器打开面板时预置 API Key，并明确提示用户已经复制成功。 */
    private void copyKeyForBrowser(String text) {
        ClipboardManager cm = (ClipboardManager) getSystemService(Context.CLIPBOARD_SERVICE);
        if (cm != null) {
            cm.setPrimaryClip(ClipData.newPlainText("wb2api API Key", text));
            toast(getString(R.string.api_key_copied));
        }
    }

    private void openUpstream() {
        openExternal(Uri.parse(UPSTREAM_URL));
    }

    private void checkForUpdates() {
        if (updateCheckRunning) {
            toast("正在检测更新…");
            return;
        }
        updateCheckRunning = true;
        toast("正在检测更新…");

        new Thread(new Runnable() {
            @Override
            public void run() {
                ReleaseInfo release = null;
                String error = null;
                try {
                    release = fetchLatestRelease();
                } catch (Exception e) {
                    error = e.getMessage();
                    Log.w(TAG, "检测上游更新失败", e);
                }

                final ReleaseInfo result = release;
                final String failure = error == null || error.isEmpty() ? "网络请求失败" : error;
                runOnUiThread(new Runnable() {
                    @Override
                    public void run() {
                        updateCheckRunning = false;
                        if (isFinishing() || isDestroyed()) {
                            return;
                        }
                        if (result == null) {
                            showUpdateError(failure);
                        } else {
                            showUpdateResult(result);
                        }
                    }
                });
            }
        }, "wb2api-update-check").start();
    }

    private ReleaseInfo fetchLatestRelease() throws Exception {
        HttpURLConnection connection = null;
        try {
            connection = (HttpURLConnection) new URL(LATEST_RELEASE_API).openConnection();
            connection.setConnectTimeout(10000);
            connection.setReadTimeout(15000);
            connection.setRequestMethod("GET");
            connection.setRequestProperty("Accept", "application/vnd.github+json");
            connection.setRequestProperty("X-GitHub-Api-Version", "2022-11-28");
            connection.setRequestProperty("User-Agent", "wb2api-android/" + appVersionName());

            int status = connection.getResponseCode();
            InputStream stream = status >= 200 && status < 300
                    ? connection.getInputStream()
                    : connection.getErrorStream();
            String body = readText(stream);
            if (status < 200 || status >= 300) {
                if (status == 403 || status == 429) {
                    throw new IOException("GitHub API 请求受限，请稍后重试");
                }
                throw new IOException("GitHub 返回 HTTP " + status);
            }

            JSONObject json = new JSONObject(body);
            String tag = json.optString("tag_name", "").trim();
            if (tag.isEmpty()) {
                throw new IOException("上游没有可用的 Release 版本");
            }
            String pageUrl = json.optString("html_url", UPSTREAM_URL + "/releases");
            String apkUrl = findApkAsset(json);
            String notes = json.optString("body", "");
            return new ReleaseInfo(tag, pageUrl, apkUrl, notes);
        } finally {
            if (connection != null) {
                connection.disconnect();
            }
        }
    }

    private String findApkAsset(JSONObject release) {
        org.json.JSONArray assets = release.optJSONArray("assets");
        if (assets == null) {
            return null;
        }
        for (int i = 0; i < assets.length(); i++) {
            JSONObject asset = assets.optJSONObject(i);
            if (asset == null) {
                continue;
            }
            String name = asset.optString("name", "");
            String url = asset.optString("browser_download_url", "");
            if (name.toLowerCase().endsWith(".apk") && !url.isEmpty()) {
                return url;
            }
        }
        return null;
    }

    private String readText(InputStream stream) throws IOException {
        if (stream == null) {
            return "";
        }
        StringBuilder text = new StringBuilder();
        try (BufferedReader reader = new BufferedReader(
                new InputStreamReader(stream, StandardCharsets.UTF_8))) {
            char[] buffer = new char[4096];
            int count;
            while ((count = reader.read(buffer)) != -1) {
                text.append(buffer, 0, count);
                if (text.length() >= 1024 * 1024) {
                    break;
                }
            }
        }
        return text.toString();
    }

    private void showUpdateResult(ReleaseInfo release) {
        String current = appVersionName();
        if (compareVersions(release.tag, current) > 0) {
            AlertDialog dialog = dialogBuilder()
                    .setTitle("发现新版本 " + release.tag)
                    .setMessage("当前版本：v" + current + "\n"
                            + "最新版本：" + release.tag + "\n\n"
                            + releaseNotes(release.notes))
                    .setNegativeButton("稍后", null)
                    .create();
            if (!release.apkUrl.isEmpty()) {
                dialog.setButton(AlertDialog.BUTTON_POSITIVE, "下载 APK",
                        (d, which) -> openExternal(Uri.parse(release.apkUrl)));
                dialog.setButton(AlertDialog.BUTTON_NEUTRAL, "发布页",
                        (d, which) -> openExternal(Uri.parse(release.pageUrl)));
            } else {
                dialog.setButton(AlertDialog.BUTTON_POSITIVE, "打开下载页",
                        (d, which) -> openExternal(Uri.parse(release.pageUrl)));
            }
            showDialog(dialog);
            return;
        }

        showDialog(dialogBuilder()
                .setTitle("已是最新版本")
                .setMessage("当前版本：v" + current + "\n最新版本：" + release.tag)
                .setPositiveButton("访问项目", (d, which) -> openUpstream())
                .setNegativeButton("关闭", null)
        );
    }

    private void showUpdateError(String error) {
        showDialog(dialogBuilder()
                .setTitle("检测更新失败")
                .setMessage(error)
                .setPositiveButton("访问项目", (d, which) -> openUpstream())
                .setNegativeButton("关闭", null)
        );
    }

    private String releaseNotes(String notes) {
        String clean = notes == null ? "" : notes.replace("\r\n", "\n").trim();
        if (clean.isEmpty()) {
            return "上游未提供更新说明。";
        }
        if (clean.length() > MAX_RELEASE_BODY_CHARS) {
            return clean.substring(0, MAX_RELEASE_BODY_CHARS) + "…";
        }
        return clean;
    }

    private String appVersionName() {
        try {
            String version = getPackageManager()
                    .getPackageInfo(getPackageName(), 0).versionName;
            return TextUtils.isEmpty(version) ? "未知" : version;
        } catch (PackageManager.NameNotFoundException e) {
            return "未知";
        }
    }

    private int compareVersions(String left, String right) {
        String[] a = normalizeVersion(left).split("\\.");
        String[] b = normalizeVersion(right).split("\\.");
        int count = Math.max(a.length, b.length);
        for (int i = 0; i < count; i++) {
            int av = i < a.length ? leadingNumber(a[i]) : 0;
            int bv = i < b.length ? leadingNumber(b[i]) : 0;
            if (av != bv) {
                return av > bv ? 1 : -1;
            }
        }
        return 0;
    }

    private String normalizeVersion(String version) {
        String value = version == null ? "" : version.trim();
        if (value.startsWith("v") || value.startsWith("V")) {
            value = value.substring(1);
        }
        int suffix = value.indexOf('-');
        if (suffix < 0) {
            suffix = value.indexOf('+');
        }
        return suffix < 0 ? value : value.substring(0, suffix);
    }

    private int leadingNumber(String value) {
        int end = 0;
        while (end < value.length() && Character.isDigit(value.charAt(end))) {
            end++;
        }
        if (end == 0) {
            return 0;
        }
        try {
            return Integer.parseInt(value.substring(0, end));
        } catch (NumberFormatException e) {
            return 0;
        }
    }

    private static final class ReleaseInfo {
        final String tag;
        final String pageUrl;
        final String apkUrl;
        final String notes;

        ReleaseInfo(String tag, String pageUrl, String apkUrl, String notes) {
            this.tag = tag;
            this.pageUrl = pageUrl;
            this.apkUrl = apkUrl;
            this.notes = notes;
        }
    }

    private void showLogs() {
        List<String> lines = LogRing.snapshot();
        final String text = lines.isEmpty() ? "（网关暂无输出）" : TextUtils.join("\n", lines);

        TextView tv = new TextView(this);
        tv.setText(text);
        tv.setTextSize(11f);
        tv.setTypeface(Typeface.MONOSPACE);
        tv.setTextColor(getColor(R.color.ink2));
        tv.setTextIsSelectable(true);
        int pad = dp(14);
        tv.setPadding(pad, pad, pad, pad);

        ScrollView scroller = new ScrollView(this);
        scroller.setFillViewport(false);
        scroller.setOverScrollMode(View.OVER_SCROLL_IF_CONTENT_SCROLLS);
        scroller.addView(tv);

        showDialog(dialogBuilder()
                .setTitle(R.string.action_logs)
                .setView(scroller)
                .setPositiveButton("复制", new android.content.DialogInterface.OnClickListener() {
                    @Override
                    public void onClick(android.content.DialogInterface d, int w) {
                        copy("wb2api 日志", text);
                    }
                })
                .setNeutralButton("清空", new android.content.DialogInterface.OnClickListener() {
                    @Override
                    public void onClick(android.content.DialogInterface d, int w) {
                        LogRing.clear();
                    }
                })
                .setNegativeButton("关闭", null)
        );
        capScrollHeight(scroller, tv);
    }

    // ── 积分明细 ──────────────────────────────────────────────────────────

    /** 一个账号的积分汇总（对应 /panel/api/packages 的 accounts[] 元素）。 */
    private static final class CreditRow {
        String uid;
        String nickname;
        String realm;
        long remain;
        long size;
        String error;

        long used() {
            return Math.max(0L, size - remain);
        }
    }

    /** 模型与档位接口的一行数据，只保留移动端展示需要的字段。 */
    private static final class ModelRow {
        String id = "";
        String name = "";
        String credits = "";
        String promoCredits = "";
        String promoLabel = "";
        String promoNote = "";
        String defaultEffort = "";
        final List<String> efforts = new ArrayList<>();
        boolean free;
        boolean supportsReasoning;
        boolean canDisableThinking;
        long contextLength;
        long maxOutputTokens;
    }

    /** 一次模型查询的完整结果，包含后端回显的当前域与可用域。 */
    private static final class ModelCatalog {
        String realm = "cn";
        boolean cnServable;
        boolean globalServable;
        final List<ModelRow> rows = new ArrayList<>();
    }

    /**
     * 积分明细弹窗：顶部三个汇总数字 + 下方账号列表。
     *
     * 数据来自 /panel/api/packages（逐账号实时查上游，比 overview 的余额新鲜），
     * 因此查询期间显示"正在查询"，且支持在弹窗内直接刷新。
     */
    private void showCreditsDialog() {
        if (!ready) {
            toast("网关尚未就绪，请稍后再试");
            return;
        }
        final LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp(16), dp(18), dp(16), dp(8));

        // 汇总区：总积分 / 已用 / 剩余 三栏等宽，数字用等宽字体保证宽度稳定。
        final LinearLayout summary = new LinearLayout(this);
        summary.setOrientation(LinearLayout.HORIZONTAL);
        summary.setBackgroundResource(R.drawable.bg_credits_summary);
        summary.setPadding(dp(8), dp(10), dp(8), dp(8));
        summary.addView(creditSummaryCell(getString(R.string.credits_total), "—"));
        summary.addView(creditSummaryCell(getString(R.string.credits_used), "—"));
        summary.addView(creditSummaryCell(getString(R.string.credits_remain), "—"));
        LinearLayout.LayoutParams summaryLp = new LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT);
        summaryLp.bottomMargin = dp(12);
        root.addView(summary, summaryLp);

        final LinearLayout list = new LinearLayout(this);
        list.setOrientation(LinearLayout.VERTICAL);
        final ScrollView scroller = new ScrollView(this);
        scroller.setFillViewport(false);
        scroller.setOverScrollMode(View.OVER_SCROLL_IF_CONTENT_SCROLLS);
        scroller.addView(list);
        root.addView(scroller, new LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT));

        final AlertDialog dialog = dialogBuilder()
                .setTitle(R.string.credits_title)
                .setView(root)
                .setNeutralButton(R.string.credits_refresh, null)
                .setNegativeButton(R.string.credits_close, null)
                .create();
        showDialog(dialog);

        // "刷新"默认会关掉 dialog，这里换成只重新拉数据不关闭。
        dialog.getButton(AlertDialog.BUTTON_NEUTRAL).setOnClickListener(
                v -> loadCredits(list, summary, scroller));
        loadCredits(list, summary, scroller);
    }

    /** 汇总区的一格：上面小标签、下面大数字。 */
    private View creditSummaryCell(String label, String value) {
        return creditMetricCell(label, value, R.color.ink, 19f);
    }

    /** 账号卡片里的一格指标：等宽、居中，避免长文本把整行挤乱。 */
    private View creditMetricCell(String label, String value, int valueColor, float valueSize) {
        LinearLayout cell = new LinearLayout(this);
        cell.setOrientation(LinearLayout.VERTICAL);
        cell.setGravity(Gravity.CENTER);
        cell.setPadding(dp(2), 0, dp(2), 0);
        cell.setLayoutParams(new LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f));

        TextView labelView = new TextView(this);
        labelView.setText(label);
        labelView.setTextColor(getColor(R.color.ink3));
        labelView.setTextSize(11);
        labelView.setGravity(Gravity.CENTER);
        cell.addView(labelView);

        TextView valueView = new TextView(this);
        valueView.setText(value);
        valueView.setTextColor(getColor(valueColor));
        valueView.setTextSize(valueSize);
        valueView.setTypeface(Typeface.MONOSPACE);
        valueView.setGravity(Gravity.CENTER);
        valueView.setPadding(0, dp(3), 0, dp(2));
        cell.addView(valueView);
        return cell;
    }

    /** 拉取积分并渲染；查询失败时在列表区给出可读原因。 */
    private void loadCredits(final LinearLayout list, final LinearLayout summary,
            final ScrollView scroller) {
        list.removeAllViews();
        TextView loading = new TextView(this);
        loading.setText(R.string.credits_loading);
        loading.setTextColor(getColor(R.color.ink3));
        loading.setPadding(0, dp(10), 0, dp(10));
        list.addView(loading);

        final String key = ServerProcess.readApiKey(this);
        new Thread(new Runnable() {
            @Override
            public void run() {
                final List<CreditRow> rows = new ArrayList<>();
                String failure = null;
                HttpURLConnection connection = null;
                try {
                    URL url = new URL(ServerProcess.baseUrl(MainActivity.this) + "/panel/api/packages");
                    connection = (HttpURLConnection) url.openConnection();
                    connection.setConnectTimeout(8000);
                    // 逐账号实时查上游：账号多时比普通接口慢得多，给足读超时。
                    connection.setReadTimeout(120000);
                    if (!key.isEmpty()) {
                        connection.setRequestProperty("Authorization", "Bearer " + key);
                    }
                    int status = connection.getResponseCode();
                    InputStream stream = status >= 200 && status < 300
                            ? connection.getInputStream()
                            : connection.getErrorStream();
                    String body = readText(stream);
                    if (status < 200 || status >= 300) {
                        failure = panelError(status, body);
                    } else {
                        JSONObject data = new JSONObject(body);
                        JSONArray accounts = data.optJSONArray("accounts");
                        for (int i = 0; accounts != null && i < accounts.length(); i++) {
                            JSONObject a = accounts.optJSONObject(i);
                            if (a == null) {
                                continue;
                            }
                            CreditRow row = new CreditRow();
                            row.uid = a.optString("uid");
                            row.nickname = a.optString("nickname");
                            row.realm = a.optString("realm");
                            row.remain = a.optLong("remain");
                            row.size = a.optLong("size");
                            row.error = a.optString("error");
                            rows.add(row);
                        }
                    }
                } catch (Exception e) {
                    failure = e.getMessage() == null || e.getMessage().isEmpty()
                            ? "请求失败" : e.getMessage();
                } finally {
                    if (connection != null) {
                        connection.disconnect();
                    }
                }
                final String error = failure;
                runOnUiThread(new Runnable() {
                    @Override
                    public void run() {
                        if (isFinishing() || isDestroyed()) {
                            return;
                        }
                        renderCredits(list, summary, scroller, rows, error);
                    }
                });
            }
        }, "wb2api-credits").start();
    }

    /**
     * 渲染积分明细。
     *
     * 账号行用「昵称 + 域标签」做标题，下面一行展开三个数字
     * （总积分 / 已用 / 剩余），不再展示资源包明细。
     */
    private void renderCredits(LinearLayout list, LinearLayout summary, ScrollView scroller,
            List<CreditRow> rows, String error) {
        list.removeAllViews();
        if (error != null) {
            TextView tv = new TextView(this);
            tv.setText(getString(R.string.credits_failed) + "：" + error);
            tv.setTextColor(getColor(R.color.bad));
            tv.setPadding(0, dp(12), 0, dp(12));
            list.addView(tv);
            capScrollHeight(scroller, list);
            return;
        }
        if (rows.isEmpty()) {
            TextView tv = new TextView(this);
            tv.setText(R.string.credits_empty);
            tv.setTextColor(getColor(R.color.ink3));
            tv.setPadding(0, dp(12), 0, dp(12));
            list.addView(tv);
            capScrollHeight(scroller, list);
            return;
        }

        // 汇总：总数 = 各账号 size 之和（总发放），已用 = size - remain，剩余 = remain。
        long totalSize = 0, totalRemain = 0;
        for (CreditRow row : rows) {
            totalSize += Math.max(0L, row.size);
            totalRemain += Math.max(0L, row.remain);
        }
        summary.removeAllViews();
        summary.addView(creditSummaryCell(getString(R.string.credits_total), formatCount(totalSize)));
        summary.addView(creditSummaryCell(getString(R.string.credits_used),
                formatCount(Math.max(0L, totalSize - totalRemain))));
        summary.addView(creditSummaryCell(getString(R.string.credits_remain), formatCount(totalRemain)));

        for (int rowIndex = 0; rowIndex < rows.size(); rowIndex++) {
            final CreditRow row = rows.get(rowIndex);
            LinearLayout card = new LinearLayout(this);
            card.setOrientation(LinearLayout.VERTICAL);
            int pad = dp(12);
            card.setPadding(dp(2), pad, dp(2), pad);
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT);
            card.setLayoutParams(lp);

            // 标题行：昵称（或 uid）+ 域标签。
            LinearLayout titleRow = new LinearLayout(this);
            titleRow.setOrientation(LinearLayout.HORIZONTAL);
            titleRow.setGravity(Gravity.CENTER_VERTICAL);

            TextView name = new TextView(this);
            String shown = row.nickname.isEmpty() ? row.uid : row.nickname;
            name.setText(shown);
            name.setTextColor(getColor(R.color.ink));
            name.setTextSize(15);
            name.setTypeface(Typeface.DEFAULT_BOLD);
            name.setSingleLine(true);
            name.setEllipsize(TextUtils.TruncateAt.END);
            LinearLayout.LayoutParams nameLp = new LinearLayout.LayoutParams(
                    dp(112), LinearLayout.LayoutParams.WRAP_CONTENT);
            name.setLayoutParams(nameLp);
            titleRow.addView(name);

            // 账号后的余额进度条：与网页账号池一致，按“剩余 / 总额”计算。
            boolean hasQuota = row.size > 0;
            int remainPercent = creditRemainPercent(row);
            ProgressBar remainBar = new ProgressBar(
                    this, null, android.R.attr.progressBarStyleHorizontal);
            remainBar.setMax(100);
            remainBar.setProgress(hasQuota ? remainPercent : 0);
            remainBar.setProgressDrawable(roundedProgressDrawable(
                    getColor(R.color.accent), getColor(R.color.line)));
            remainBar.setMinimumHeight(dp(6));
            LinearLayout.LayoutParams remainBarLp = new LinearLayout.LayoutParams(
                    dp(96), dp(6));
            remainBarLp.setMarginStart(dp(8));
            remainBarLp.setMarginEnd(dp(6));
            remainBar.setLayoutParams(remainBarLp);
            titleRow.addView(remainBar);

            TextView percent = new TextView(this);
            percent.setText(hasQuota ? remainPercent + "%" : "—");
            percent.setTextColor(getColor(R.color.ink3));
            percent.setTextSize(11);
            percent.setTypeface(Typeface.MONOSPACE);
            percent.setGravity(Gravity.CENTER);
            percent.setSingleLine(true);
            percent.setContentDescription(hasQuota
                    ? getString(R.string.credits_remain) + " " + remainPercent + "%"
                    : getString(R.string.credits_remain) + " —");
            LinearLayout.LayoutParams percentLp = new LinearLayout.LayoutParams(
                    dp(36),
                    LinearLayout.LayoutParams.WRAP_CONTENT);
            percentLp.setMarginEnd(dp(6));
            percent.setLayoutParams(percentLp);
            titleRow.addView(percent);

            String realmText = realmLabel(row.realm);
            if (!"—".equals(realmText)) {
                TextView realm = new TextView(this);
                realm.setText(realmText);
                realm.setTextColor(getColor(R.color.ink3));
                realm.setTextSize(10);
                realm.setBackgroundResource(R.drawable.bg_field_outline);
                int rp = dp(7);
                realm.setPadding(rp, dp(2), rp, dp(2));
                titleRow.addView(realm);
            }
            card.addView(titleRow);

            // 三列等宽指标：总积分 / 已用 / 剩余。
            LinearLayout metrics = new LinearLayout(this);
            metrics.setOrientation(LinearLayout.HORIZONTAL);
            metrics.setPadding(0, dp(10), 0, 0);
            metrics.addView(creditMetricCell(getString(R.string.credits_total),
                    formatCount(row.size), R.color.ink, 16f));
            metrics.addView(creditMetricCell(getString(R.string.credits_used),
                    formatCount(row.used()), R.color.ink, 16f));
            metrics.addView(creditMetricCell(getString(R.string.credits_remain),
                    formatCount(row.remain), R.color.ink, 16f));
            card.addView(metrics);

            if (!row.error.isEmpty()) {
                TextView err = new TextView(this);
                err.setText(row.error);
                err.setTextColor(getColor(R.color.bad));
                err.setTextSize(11);
                err.setPadding(0, dp(6), 0, 0);
                card.addView(err);
            }

            list.addView(card);

            if (rowIndex < rows.size() - 1) {
                View divider = new View(this);
                divider.setBackgroundColor(getColor(R.color.line));
                LinearLayout.LayoutParams dividerLp = new LinearLayout.LayoutParams(
                        LinearLayout.LayoutParams.MATCH_PARENT, dp(1));
                divider.setLayoutParams(dividerLp);
                list.addView(divider);
            }
        }
        capScrollHeight(scroller, list);
    }

    private int creditRemainPercent(CreditRow row) {
        if (row.size <= 0) {
            return 0;
        }
        long pct = Math.round(row.remain * 100.0 / row.size);
        return (int) Math.max(0L, Math.min(100L, pct));
    }

    private LayerDrawable roundedProgressDrawable(int progressColor, int trackColor) {
        float radius = dp(3);

        GradientDrawable track = new GradientDrawable();
        track.setColor(trackColor);
        track.setCornerRadius(radius);

        GradientDrawable progress = new GradientDrawable();
        progress.setColor(progressColor);
        progress.setCornerRadius(radius);

        ClipDrawable clip = new ClipDrawable(progress, Gravity.START, ClipDrawable.HORIZONTAL);
        LayerDrawable drawable = new LayerDrawable(new android.graphics.drawable.Drawable[]{track, clip});
        drawable.setId(0, android.R.id.background);
        drawable.setId(1, android.R.id.progress);
        return drawable;
    }

    private String realmLabel(String realm) {
        if ("global".equals(realm)) {
            return getString(R.string.credits_realm_global);
        }
        if ("cn".equals(realm)) {
            return getString(R.string.credits_realm_cn);
        }
        return "—";
    }

    private String modelRealmLabel(String realm) {
        if ("global".equals(realm)) {
            return getString(R.string.models_realm_global);
        }
        return getString(R.string.models_realm_cn);
    }

    /**
     * 模型与档位弹窗：顶部切换国内服/国际服，下面按上游返回的模型列表展示
     * 倍率、默认档、支持档位、上下文和最大输出。默认固定请求国内服。
     */
    private void showModelsDialog() {
        if (!ready) {
            toast("网关尚未就绪，请稍后再试");
            return;
        }

        final LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        int pad = dp(20);
        root.setPadding(pad, dp(18), pad, dp(8));

        final LinearLayout realmRow = new LinearLayout(this);
        realmRow.setOrientation(LinearLayout.HORIZONTAL);
        realmRow.setPadding(0, 0, 0, dp(12));
        final TextView cnChip = modelRealmChip(R.string.models_realm_cn);
        final TextView globalChip = modelRealmChip(R.string.models_realm_global);
        realmRow.addView(cnChip);
        realmRow.addView(globalChip);
        root.addView(realmRow);

        final TextView status = new TextView(this);
        status.setText(R.string.models_realm_hint);
        status.setTextColor(getColor(R.color.ink3));
        status.setTextSize(12);
        status.setPadding(dp(2), 0, dp(2), dp(10));
        root.addView(status);

        final LinearLayout list = new LinearLayout(this);
        list.setOrientation(LinearLayout.VERTICAL);
        final ScrollView scroller = new ScrollView(this);
        scroller.setFillViewport(false);
        scroller.setOverScrollMode(View.OVER_SCROLL_IF_CONTENT_SCROLLS);
        scroller.addView(list);
        root.addView(scroller, new LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT));

        final AlertDialog dialog = dialogBuilder()
                .setTitle(R.string.models_title)
                .setView(root)
                .setNeutralButton(R.string.models_refresh, null)
                .setNegativeButton(R.string.models_close, null)
                .create();
        showDialog(dialog);

        final String[] selectedRealm = {"cn"};
        final int[] sequence = {0};
        cnChip.setOnClickListener(v -> {
            if ("cn".equals(selectedRealm[0])) {
                return;
            }
            selectedRealm[0] = "cn";
            updateModelRealmChips(cnChip, globalChip, "cn", true, true);
            loadModels("cn", list, status, scroller, cnChip, globalChip, sequence);
        });
        globalChip.setOnClickListener(v -> {
            if ("global".equals(selectedRealm[0])) {
                return;
            }
            selectedRealm[0] = "global";
            updateModelRealmChips(cnChip, globalChip, "global", true, true);
            loadModels("global", list, status, scroller, cnChip, globalChip, sequence);
        });
        dialog.getButton(AlertDialog.BUTTON_NEUTRAL).setOnClickListener(
                v -> loadModels(selectedRealm[0], list, status, scroller, cnChip, globalChip, sequence));

        updateModelRealmChips(cnChip, globalChip, "cn", true, true);
        loadModels("cn", list, status, scroller, cnChip, globalChip, sequence);
    }

    /** 模型域切换按钮，等宽横排。 */
    private TextView modelRealmChip(int textRes) {
        TextView chip = new TextView(this);
        chip.setText(textRes);
        chip.setTextSize(13);
        chip.setTypeface(Typeface.DEFAULT_BOLD);
        chip.setGravity(Gravity.CENTER);
        chip.setMinHeight(dp(40));
        chip.setPadding(dp(12), 0, dp(12), 0);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                0, dp(40), 1f);
        if (textRes == R.string.models_realm_global) {
            lp.setMarginStart(dp(8));
        }
        chip.setLayoutParams(lp);
        return chip;
    }

    /** 切换按钮的选中态；不可用域降透明度提示，但仍允许点击查看错误原因。 */
    private void updateModelRealmChips(TextView cnChip, TextView globalChip, String selected,
            boolean cnServable, boolean globalServable) {
        styleModelRealmChip(cnChip, "cn".equals(selected), cnServable);
        styleModelRealmChip(globalChip, "global".equals(selected), globalServable);
    }

    private void styleModelRealmChip(TextView chip, boolean selected, boolean servable) {
        if (chip == null) {
            return;
        }
        chip.setBackgroundResource(selected
                ? R.drawable.bg_realm_chip_selected
                : R.drawable.bg_realm_chip);
        chip.setTextColor(selected ? 0xFFFFFFFF : getColor(R.color.ink));
        chip.setAlpha(servable ? 1f : 0.45f);
    }

    /** 拉取指定域的模型列表；用 sequence 丢弃快速切换产生的旧响应。 */
    private void loadModels(final String realm, final LinearLayout list, final TextView status,
            final ScrollView scroller, final TextView cnChip, final TextView globalChip,
            final int[] sequence) {
        final int requestId = ++sequence[0];
        list.removeAllViews();
        TextView loading = new TextView(this);
        loading.setText(R.string.models_loading);
        loading.setTextColor(getColor(R.color.ink3));
        loading.setPadding(dp(2), dp(10), dp(2), dp(10));
        list.addView(loading);
        status.setText(modelRealmLabel(realm) + " · " + getString(R.string.models_loading));

        final String key = ServerProcess.readApiKey(this);
        new Thread(new Runnable() {
            @Override
            public void run() {
                ModelCatalog catalog = null;
                String failure = null;
                HttpURLConnection connection = null;
                try {
                    URL url = new URL(ServerProcess.baseUrl(MainActivity.this)
                            + "/panel/api/models?realm=" + realm);
                    connection = (HttpURLConnection) url.openConnection();
                    connection.setConnectTimeout(8000);
                    connection.setReadTimeout(30000);
                    if (!key.isEmpty()) {
                        connection.setRequestProperty("Authorization", "Bearer " + key);
                    }
                    int code = connection.getResponseCode();
                    InputStream stream = code >= 200 && code < 300
                            ? connection.getInputStream()
                            : connection.getErrorStream();
                    String body = readText(stream);
                    if (code < 200 || code >= 300) {
                        failure = panelError(code, body);
                    } else {
                        catalog = parseModelCatalog(new JSONObject(body));
                    }
                } catch (Exception e) {
                    failure = e.getMessage() == null || e.getMessage().isEmpty()
                            ? "请求失败" : e.getMessage();
                } finally {
                    if (connection != null) {
                        connection.disconnect();
                    }
                }
                final ModelCatalog result = catalog;
                final String error = failure;
                runOnUiThread(new Runnable() {
                    @Override
                    public void run() {
                        if (isFinishing() || isDestroyed() || requestId != sequence[0]) {
                            return;
                        }
                        renderModels(list, status, scroller, cnChip, globalChip, result, error);
                    }
                });
            }
        }, "wb2api-models").start();
    }

    private ModelCatalog parseModelCatalog(JSONObject data) {
        ModelCatalog catalog = new ModelCatalog();
        catalog.realm = data.optString("realm", "cn");
        catalog.cnServable = true;
        catalog.globalServable = true;
        JSONObject servable = data.optJSONObject("realm_servable");
        if (servable != null) {
            catalog.cnServable = servable.optBoolean("cn", true);
            catalog.globalServable = servable.optBoolean("global", true);
        }

        JSONArray models = data.optJSONArray("models");
        for (int i = 0; models != null && i < models.length(); i++) {
            JSONObject item = models.optJSONObject(i);
            if (item == null) {
                continue;
            }
            ModelRow row = new ModelRow();
            row.id = item.optString("id", "");
            row.name = item.optString("name", "");
            row.credits = item.optString("credits", "");
            row.promoCredits = item.optString("promo_credits", "");
            row.promoLabel = cleanPromoLabel(item.optString("promo_label", ""));
            row.promoNote = item.optString("promo_note", "");
            row.defaultEffort = item.optString("default_effort", "");
            row.free = item.optBoolean("free", false);
            row.supportsReasoning = item.optBoolean("supports_reasoning", false);
            row.canDisableThinking = item.optBoolean("can_disable_thinking", false);
            row.contextLength = item.optLong("context_length", 0L);
            row.maxOutputTokens = item.optLong("max_output_tokens", 0L);
            JSONArray efforts = item.optJSONArray("supported_efforts");
            for (int j = 0; efforts != null && j < efforts.length(); j++) {
                String effort = efforts.optString(j, "");
                if (!effort.isEmpty()) {
                    row.efforts.add(effort);
                }
            }
            catalog.rows.add(row);
        }
        return catalog;
    }

    private void renderModels(LinearLayout list, TextView status, ScrollView scroller,
            TextView cnChip, TextView globalChip, ModelCatalog catalog, String error) {
        list.removeAllViews();
        String realm = catalog == null ? "cn" : catalog.realm;
        boolean cnServable = catalog == null || catalog.cnServable;
        boolean globalServable = catalog == null || catalog.globalServable;
        updateModelRealmChips(cnChip, globalChip, realm, cnServable, globalServable);

        if (error != null) {
            status.setText(modelRealmLabel(realm) + " · " + getString(R.string.models_failed));
            TextView tv = new TextView(this);
            tv.setText(getString(R.string.models_failed) + "：" + error);
            tv.setTextColor(getColor(R.color.bad));
            tv.setPadding(dp(2), dp(12), dp(2), dp(12));
            list.addView(tv);
            capScrollHeight(scroller, list);
            return;
        }
        if (catalog == null || catalog.rows.isEmpty()) {
            status.setText(getString(R.string.models_realm_status,
                    modelRealmLabel(realm), 0));
            TextView tv = new TextView(this);
            tv.setText(R.string.models_empty);
            tv.setTextColor(getColor(R.color.ink3));
            tv.setPadding(dp(2), dp(12), dp(2), dp(12));
            list.addView(tv);
            capScrollHeight(scroller, list);
            return;
        }

        status.setText(getString(R.string.models_realm_status,
                modelRealmLabel(realm), catalog.rows.size()));
        // 免费模型优先展示，方便用户一眼找到不消耗积分的可用模型。
        List<ModelRow> rows = new ArrayList<>(catalog.rows);
        rows.sort((a, b) -> Boolean.compare(b.free, a.free));
        for (int i = 0; i < rows.size(); i++) {
            list.addView(modelRowView(rows.get(i)));
            if (i < rows.size() - 1) {
                View divider = new View(this);
                divider.setBackgroundColor(getColor(R.color.line));
                LinearLayout.LayoutParams dividerLp = new LinearLayout.LayoutParams(
                        LinearLayout.LayoutParams.MATCH_PARENT, dp(1));
                dividerLp.setMarginStart(dp(12));
                dividerLp.setMarginEnd(dp(12));
                divider.setLayoutParams(dividerLp);
                list.addView(divider);
            }
        }
        animateModelList(list);
        capScrollHeight(scroller, list);
    }

    /** 列表加载后的轻量动效：逐行淡入并轻微上移，避免一次性生硬出现。 */
    private void animateModelList(LinearLayout list) {
        int rowIndex = 0;
        for (int i = 0; i < list.getChildCount(); i++) {
            View child = list.getChildAt(i);
            if (!(child instanceof LinearLayout)) {
                continue;
            }
            child.setAlpha(0f);
            child.setTranslationY(dp(8));
            child.animate()
                    .alpha(1f)
                    .translationY(0f)
                    .setStartDelay(Math.min(rowIndex * 28, 280))
                    .setDuration(220)
                    .start();
            rowIndex++;
        }
    }

    private View modelRowView(ModelRow row) {
        LinearLayout block = new LinearLayout(this);
        block.setOrientation(LinearLayout.HORIZONTAL);
        block.setGravity(Gravity.TOP);
        block.setPadding(dp(2), dp(9), dp(2), dp(9));
        if (row.free) {
            block.setBackgroundResource(R.drawable.bg_model_row_tint);
        }

        // 标记条使用固定高度，放在整行高度的槽位中垂直居中。
        LinearLayout markerSlot = new LinearLayout(this);
        markerSlot.setOrientation(LinearLayout.VERTICAL);
        markerSlot.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams markerSlotLp = new LinearLayout.LayoutParams(
                dp(4), LinearLayout.LayoutParams.MATCH_PARENT);
        markerSlotLp.setMarginEnd(dp(7));
        markerSlot.setLayoutParams(markerSlotLp);

        View marker = new View(this);
        marker.setLayoutParams(new LinearLayout.LayoutParams(dp(4), dp(30)));
        if (row.free) {
            marker.setBackgroundResource(R.drawable.bg_model_free_marker);
        }
        markerSlot.addView(marker);
        block.addView(markerSlot);

        // 左栏只放模型身份，右栏放紧凑的档位与价格信息，避免每项纵向拉得太长。
        LinearLayout identity = new LinearLayout(this);
        identity.setOrientation(LinearLayout.VERTICAL);
        LinearLayout.LayoutParams identityLp = new LinearLayout.LayoutParams(
                0, LinearLayout.LayoutParams.WRAP_CONTENT, 0.38f);
        identity.setLayoutParams(identityLp);

        TextView id = new TextView(this);
        id.setText(row.id);
        id.setTextColor(getColor(R.color.ink));
        id.setTextSize(14);
        id.setTypeface(Typeface.DEFAULT_BOLD);
        id.setMaxLines(2);
        id.setEllipsize(TextUtils.TruncateAt.END);
        identity.addView(id);
        if (!row.name.isEmpty() && !row.name.equals(row.id)) {
            TextView name = new TextView(this);
            name.setText(row.name);
            name.setTextColor(getColor(R.color.ink3));
            name.setTextSize(10);
            name.setMaxLines(2);
            name.setEllipsize(TextUtils.TruncateAt.END);
            name.setPadding(0, dp(2), 0, 0);
            identity.addView(name);
        }
        TextView defaultEffort = new TextView(this);
        defaultEffort.setText(getString(R.string.models_default_effort) + "："
                + (row.defaultEffort.isEmpty() ? "—" : row.defaultEffort));
        defaultEffort.setTextColor(getColor(R.color.ink2));
        defaultEffort.setTextSize(11);
        defaultEffort.setPadding(0, dp(3), 0, 0);
        identity.addView(defaultEffort);
        block.addView(identity);

        LinearLayout details = new LinearLayout(this);
        details.setOrientation(LinearLayout.VERTICAL);
        LinearLayout.LayoutParams detailsLp = new LinearLayout.LayoutParams(
                0, LinearLayout.LayoutParams.WRAP_CONTENT, 0.62f);
        detailsLp.setMarginStart(dp(12));
        details.setLayoutParams(detailsLp);
        details.addView(modelCompactLine(getString(R.string.models_rate),
                modelRateText(row), true));
        details.addView(modelCompactLine(getString(R.string.models_efforts),
                modelEffortsText(row), false));
        details.addView(modelCompactLine(getString(R.string.models_context),
                formatTokenLimit(row.contextLength), false));
        block.addView(details);
        return block;
    }

    /** 右侧详情的一行：标签在左、值在右，行高固定得尽量紧凑。 */
    private View modelCompactLine(String label, String value, boolean accentValue) {
        LinearLayout line = new LinearLayout(this);
        line.setOrientation(LinearLayout.HORIZONTAL);
        line.setGravity(Gravity.TOP);
        line.setPadding(0, dp(1), 0, dp(1));

        TextView labelView = new TextView(this);
        labelView.setText(label);
        labelView.setTextColor(getColor(R.color.ink3));
        labelView.setTextSize(10);
        LinearLayout.LayoutParams labelLp = new LinearLayout.LayoutParams(
                dp(48), LinearLayout.LayoutParams.WRAP_CONTENT);
        labelView.setLayoutParams(labelLp);
        line.addView(labelView);

        TextView valueView = new TextView(this);
        valueView.setText(value);
        valueView.setTextColor(getColor(accentValue ? R.color.accent : R.color.ink));
        valueView.setTextSize(12);
        valueView.setTypeface(accentValue ? Typeface.DEFAULT_BOLD : Typeface.MONOSPACE);
        valueView.setLineSpacing(dp(1), 1f);
        LinearLayout.LayoutParams valueLp = new LinearLayout.LayoutParams(
                0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f);
        valueView.setLayoutParams(valueLp);
        line.addView(valueView);
        return line;
    }

    private String modelRateText(ModelRow row) {
        String effective = row.promoCredits.trim();
        String base = row.credits.trim();
        if (!effective.isEmpty()) {
            StringBuilder text = new StringBuilder(effective);
            if (!row.promoLabel.isEmpty()) {
                text.append(" · ").append(row.promoLabel);
            }
            return text.toString();
        }
        if (!base.isEmpty()) {
            return row.free ? base + " · 免费" : base;
        }
        return row.free ? "免费" : "—";
    }

    /**
     * 只保留优惠名称，去掉接口文案里的括号和原价说明，避免积分倍率行被撑高。
     */
    private String cleanPromoLabel(String label) {
        if (label == null) {
            return "";
        }
        String text = label.trim();
        int originalPrice = text.indexOf("原价");
        if (originalPrice >= 0) {
            text = text.substring(0, originalPrice).trim();
        }
        text = text.replace("【", "")
                .replace("】", "")
                .replace("[", "")
                .replace("]", "")
                .trim();
        while (!text.isEmpty()) {
            char last = text.charAt(text.length() - 1);
            if (last == '(' || last == '（' || last == '·'
                    || last == '-' || last == '—' || last == '/' || last == ' ') {
                text = text.substring(0, text.length() - 1).trim();
            } else {
                break;
            }
        }
        return text;
    }

    private String modelEffortsText(ModelRow row) {
        if (!row.efforts.isEmpty()) {
            String text = TextUtils.join(" / ", row.efforts);
            if (row.canDisableThinking && !row.efforts.contains("off")) {
                text += " / " + getString(R.string.models_off_effort);
            }
            return text;
        }
        if (row.supportsReasoning) {
            return getString(R.string.models_reasoning_fixed,
                    row.defaultEffort.isEmpty() ? "?" : row.defaultEffort);
        }
        return getString(R.string.models_reasoning_none);
    }

    private String formatTokenLimit(long n) {
        if (n <= 0) {
            return "—";
        }
        if (n < 1000) {
            return String.valueOf(n);
        }
        return Math.round(n / 1000.0) + "K";
    }

    /** 大数字缩写：1.2k / 3.4M，和面板的 fmtTok 同口径，避免窄屏里数字换行。 */
    private String formatCount(long n) {
        if (n < 0) {
            n = 0;
        }
        if (n < 10000) {
            return String.valueOf(n);
        }
        if (n < 1000000) {
            return trimZero(n / 1000.0) + "k";
        }
        return trimZero(n / 1000000.0) + "M";
    }

    private String trimZero(double v) {
        String s = String.format(Locale.US, "%.1f", v);
        return s.endsWith(".0") ? s.substring(0, s.length() - 2) : s;
    }

    private void copyAccessInfo() {
        String key = ServerProcess.readApiKey(this);

        StringBuilder sb = new StringBuilder();
        sb.append("控制台: ").append(consoleUrl()).append('\n');
        String wifi = wifiUrl();
        if (wifi != null) {
            sb.append("局域网 Base URL: ").append(wifi).append("/v1\n");
        } else {
            sb.append("局域网访问: 未开启（设置里可改）\n");
        }
        sb.append("API Key: ").append(key.isEmpty() ? "(尚未生成)" : key);
        copy("wb2api 访问信息", sb.toString());
    }

    private void showSettings() {
        View root = getLayoutInflater().inflate(R.layout.dialog_settings, null);
        final Switch lanSwitch = root.findViewById(R.id.settingsLanSwitch);
        final Switch bootSwitch = root.findViewById(R.id.settingsBootSwitch);
        final Switch notifySwitch = root.findViewById(R.id.settingsNotifySwitch);
        final View lanRow = root.findViewById(R.id.settingsLanRow);
        final View bootRow = root.findViewById(R.id.settingsBootRow);
        final View notifyRow = root.findViewById(R.id.settingsNotifyRow);
        final View batteryRow = root.findViewById(R.id.settingsBatteryRow);

        lanSwitch.setChecked(Prefs.lan(this));
        bootSwitch.setChecked(Prefs.autoStart(this));
        notifySwitch.setChecked(Prefs.notify(this));

        lanRow.setOnClickListener(v -> lanSwitch.setChecked(!lanSwitch.isChecked()));
        bootRow.setOnClickListener(v -> bootSwitch.setChecked(!bootSwitch.isChecked()));
        notifyRow.setOnClickListener(v -> notifySwitch.setChecked(!notifySwitch.isChecked()));

        AlertDialog dialog = dialogBuilder()
                .setTitle(R.string.action_settings)
                .setView(root)
                .setPositiveButton(R.string.settings_save_restart, new android.content.DialogInterface.OnClickListener() {
                    @Override
                    public void onClick(android.content.DialogInterface d, int w) {
                        Prefs.setLan(MainActivity.this, lanSwitch.isChecked());
                        Prefs.setAutoStart(MainActivity.this, bootSwitch.isChecked());
                        Prefs.setNotify(MainActivity.this, notifySwitch.isChecked());
                        seeded = false;
                        clearWebCache();
                        GatewayService.restart(MainActivity.this);
                        if (!notifySwitch.isChecked()) {
                            toast(getString(R.string.notify_off_toast));
                        }
                    }
                })
                .setNegativeButton("取消", null)
                .create();

        batteryRow.setOnClickListener(v -> {
            dialog.dismiss();
            openBatterySettings();
        });
        showDialog(dialog);
    }

    private void openBatterySettings() {
        try {
            startActivity(new Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS));
        } catch (Exception e) {
            try {
                startActivity(new Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                        Uri.parse("package:" + getPackageName())));
            } catch (Exception ignored) {
                toast("无法打开系统设置");
            }
        }
    }

    private void showAbout() {
        String ip = LocalIp.get();
        int port = Prefs.port(this);
        String text = "WorkBuddy 网关：v" + appVersionName() + "\n"
                + "运行架构：" + Build.SUPPORTED_ABIS[0] + "\n"
                + "监听端口：" + port + (Prefs.lan(this) ? "（含局域网）" : "（仅本机）") + "\n"
                + (ip == null ? "" : "本机 IP：" + ip + "\n")
                + "数据目录：" + ServerProcess.workDir(this).getAbsolutePath() + "\n\n"
                + "账号、代理配置等全部写在服务器端 config.json / state.json 里，"
                + "与桌面版完全一致；换设备时把 config.json 和 auths 目录一并拷过去即可。\n\n"
                + "上游项目：" + UPSTREAM_URL;
        showDialog(dialogBuilder()
                .setTitle(R.string.action_about)
                .setMessage(text)
                .setPositiveButton(R.string.action_update,
                        (dialog, which) -> checkForUpdates())
                .setNeutralButton(R.string.action_upstream,
                        (dialog, which) -> openUpstream())
                .setNegativeButton("关闭", null)
        );
    }

    private AlertDialog.Builder dialogBuilder() {
        return new AlertDialog.Builder(this, R.style.AppDialogTheme);
    }

    private AlertDialog showDialog(AlertDialog.Builder builder) {
        AlertDialog dialog = builder.create();
        showDialog(dialog);
        return dialog;
    }

    private void showDialog(AlertDialog dialog) {
        dialog.show();
        styleDialog(dialog);
    }

    /** 所有弹窗统一使用大圆角、接近满宽的尺寸，并保持内容高度自适应。 */
    private void styleDialog(AlertDialog dialog) {
        Window window = dialog == null ? null : dialog.getWindow();
        if (window == null) {
            return;
        }
        window.setBackgroundDrawableResource(R.drawable.bg_dialog);
        window.setGravity(Gravity.CENTER);
        window.addFlags(WindowManager.LayoutParams.FLAG_DIM_BEHIND);
        window.setDimAmount(0.45f);
        int width = Math.min(
                Math.round(getResources().getDisplayMetrics().widthPixels * 0.92f),
                dp(620));
        window.setLayout(width, WindowManager.LayoutParams.WRAP_CONTENT);
    }

    /** 滚动内容短时贴合内容高度，内容长时限制最大高度，避免整屏空白。 */
    private void capScrollHeight(final ScrollView scroller, final View content) {
        final int maxHeight = Math.min(
                dp(520),
                Math.round(getResources().getDisplayMetrics().heightPixels * 0.62f));
        ViewTreeObserver observer = content.getViewTreeObserver();
        if (!observer.isAlive()) {
            return;
        }
        observer.addOnPreDrawListener(new ViewTreeObserver.OnPreDrawListener() {
            @Override
            public boolean onPreDraw() {
                ViewTreeObserver current = content.getViewTreeObserver();
                if (current.isAlive()) {
                    current.removeOnPreDrawListener(this);
                }
                int contentHeight = content.getMeasuredHeight();
                if (contentHeight <= 0) {
                    return true;
                }
                ViewGroup.LayoutParams lp = scroller.getLayoutParams();
                if (lp == null) {
                    lp = scroller.getParent() instanceof ViewGroup
                            ? ((ViewGroup) scroller.getParent()).generateLayoutParams(null)
                            : new ViewGroup.LayoutParams(
                                    ViewGroup.LayoutParams.MATCH_PARENT,
                                    ViewGroup.LayoutParams.WRAP_CONTENT);
                }
                int desired = Math.min(contentHeight, maxHeight);
                if (lp.height != desired) {
                    lp.height = desired;
                    scroller.setLayoutParams(lp);
                }
                return true;
            }
        });
    }

    private void copy(String label, String text) {
        ClipboardManager cm = (ClipboardManager) getSystemService(Context.CLIPBOARD_SERVICE);
        if (cm != null) {
            cm.setPrimaryClip(ClipData.newPlainText(label, text));
            toast("已复制到剪贴板");
        }
    }

    private int dp(int v) {
        return Math.round(v * getResources().getDisplayMetrics().density);
    }

    private void toast(String s) {
        Toast.makeText(this, s, Toast.LENGTH_SHORT).show();
    }

    // ── 面板导出桥 ───────────────────────────────────────────────────────

    /**
     * 面板把导出内容回传给原生层，由系统文件选择器决定保存位置。
     * 不用 WebView 的 blob/data 下载：DownloadListener 拿不到内容，Android WebView
     * 也不会为它们弹出可写的系统保存界面。
     */
    private final class ExportBridge {
        @JavascriptInterface
        public boolean systemDark() {
            int mode = getResources().getConfiguration().uiMode & Configuration.UI_MODE_NIGHT_MASK;
            return mode == Configuration.UI_MODE_NIGHT_YES;
        }

        @JavascriptInterface
        public boolean saveJson(String filename, String content) {
            if (content == null) {
                runOnUiThread(new Runnable() {
                    @Override
                    public void run() {
                        toast("导出内容为空");
                    }
                });
                return true;
            }
            if (content.getBytes(StandardCharsets.UTF_8).length > MAX_EXPORT_BYTES) {
                runOnUiThread(new Runnable() {
                    @Override
                    public void run() {
                        toast("导出文件过大，最大支持 32 MB");
                    }
                });
                return true;
            }

            final String safeName = safeExportName(filename);
            synchronized (exportLock) {
                if (pendingExportContent != null) {
                    runOnUiThread(new Runnable() {
                        @Override
                        public void run() {
                            toast("已有导出任务正在进行");
                        }
                    });
                    return true;
                }
                pendingExportName = safeName;
                pendingExportContent = content;
            }

            runOnUiThread(new Runnable() {
                @Override
                public void run() {
                    String current = web == null ? null : web.getUrl();
                    if (current == null
                            || !current.startsWith(ServerProcess.baseUrl(MainActivity.this))) {
                        clearPendingExport();
                        toast("导出仅支持本地面板");
                        return;
                    }
                    Intent save = new Intent(Intent.ACTION_CREATE_DOCUMENT);
                    save.addCategory(Intent.CATEGORY_OPENABLE);
                    save.setType("application/json");
                    save.putExtra(Intent.EXTRA_TITLE, safeName);
                    save.addFlags(Intent.FLAG_GRANT_WRITE_URI_PERMISSION);
                    try {
                        startActivityForResult(save, REQ_EXPORT);
                    } catch (ActivityNotFoundException e) {
                        clearPendingExport();
                        toast("没有可保存文件的应用");
                    }
                }
            });
            return true;
        }
    }

    private String safeExportName(String filename) {
        String name = filename == null ? "" : filename.trim();
        name = name.replaceAll("[\\\\/:*?\"<>|\\p{Cntrl}]", "_");
        if (name.length() > 120) {
            name = name.substring(0, 120);
        }
        if (name.isEmpty() || ".".equals(name) || "..".equals(name)) {
            name = "wb2api-accounts.json";
        }
        if (!name.toLowerCase().endsWith(".json")) {
            name += ".json";
        }
        return name;
    }

    private void clearPendingExport() {
        synchronized (exportLock) {
            pendingExportName = null;
            pendingExportContent = null;
        }
    }

    private void handleExportResult(int resultCode, Intent data) {
        final String filename;
        final String content;
        synchronized (exportLock) {
            filename = pendingExportName == null ? "wb2api-accounts.json" : pendingExportName;
            content = pendingExportContent;
            pendingExportName = null;
            pendingExportContent = null;
        }
        if (resultCode != RESULT_OK || data == null || data.getData() == null) {
            toast("已取消导出");
            return;
        }
        if (content == null) {
            toast("导出内容已失效，请重试");
            return;
        }
        final Uri uri = data.getData();
        new Thread(new Runnable() {
            @Override
            public void run() {
                String error = null;
                try (OutputStream out = getContentResolver().openOutputStream(uri)) {
                    if (out == null) {
                        throw new IllegalStateException("openOutputStream returned null");
                    }
                    out.write(content.getBytes(StandardCharsets.UTF_8));
                    out.flush();
                } catch (Exception e) {
                    error = e.getMessage();
                    Log.w(TAG, "写导出文件失败", e);
                }
                final String message = error == null
                        ? "已保存 " + filename
                        : "保存失败：" + (error == null ? "未知错误" : error);
                runOnUiThread(new Runnable() {
                    @Override
                    public void run() {
                        toast(message);
                    }
                });
            }
        }).start();
    }

    // ── 权限与结果回调 ─────────────────────────────────────────────────────

    private void requestNotificationPermission() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) {
            return;
        }
        if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, REQ_NOTIFICATION);
        }
    }

    /**
     * 用户在设置里补授权后回来：网关服务可能已经在跑，而它的常驻通知之前被系统静默丢弃了，
     * 此时补发一次启动意图，让 GatewayService 重新把通知贴出来（自身幂等，已在跑时空操作）。
     */
    @Override
    public void onRequestPermissionsResult(int requestCode, String[] permissions, int[] grantResults) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults);
        if (requestCode != REQ_NOTIFICATION) {
            return;
        }
        if (grantResults.length > 0 && grantResults[0] == PackageManager.PERMISSION_GRANTED) {
            GatewayService.start(this);
        }
    }

    @Override
    @SuppressWarnings("deprecation")
    protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode == REQ_EXPORT) {
            handleExportResult(resultCode, data);
            return;
        }
        if (requestCode != REQ_FILE) {
            return;
        }
        if (fileCallback == null) {
            return;
        }
        Uri[] result = null;
        if (resultCode == RESULT_OK && data != null) {
            if (data.getClipData() != null) {
                ClipData clip = data.getClipData();
                result = new Uri[clip.getItemCount()];
                for (int i = 0; i < clip.getItemCount(); i++) {
                    result[i] = clip.getItemAt(i).getUri();
                }
            } else if (data.getData() != null) {
                result = new Uri[]{data.getData()};
            }
        }
        try {
            fileCallback.onReceiveValue(result);
        } catch (Exception e) {
            Log.w(TAG, "回传文件选择结果失败", e);
        }
        fileCallback = null;
    }
}
