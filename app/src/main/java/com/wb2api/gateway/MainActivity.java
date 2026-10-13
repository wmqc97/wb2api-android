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
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.provider.Settings;
import android.text.InputType;
import android.text.TextUtils;
import android.util.Log;
import android.view.View;
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
import android.widget.CheckBox;
import android.widget.EditText;
import android.widget.ImageButton;
import android.widget.LinearLayout;
import android.widget.PopupMenu;
import android.widget.ProgressBar;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.util.List;

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

        GatewayState.addListener(this);
        GatewayService.start(this);
        requestNotificationPermission();
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
        // 面板内部导航优先，退到底再退 App（网关继续在后台跑）。
        if (web != null && web.canGoBack()) {
            web.goBack();
            return;
        }
        super.onBackPressed();
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
        PopupMenu menu = new PopupMenu(this, menuButton);
        menu.getMenu().add(0, 1, 0, R.string.action_reload);
        menu.getMenu().add(0, 10, 1, R.string.action_checkin_all);
        menu.getMenu().add(0, 11, 2, R.string.action_run_all_tasks);
        menu.getMenu().add(0, 8, 3, R.string.action_open_browser);
        menu.getMenu().add(0, 4, 4, R.string.action_copy);
        menu.getMenu().add(0, 5, 5, R.string.action_copy_key);
        menu.getMenu().add(0, 3, 6, R.string.action_logs);
        menu.getMenu().add(0, 2, 7, R.string.action_restart);
        menu.getMenu().add(0, 7, 8, R.string.action_battery);
        menu.getMenu().add(0, 6, 9, R.string.action_settings);
        menu.getMenu().add(0, 9, 10, R.string.action_about);
        menu.setOnMenuItemClickListener(new PopupMenu.OnMenuItemClickListener() {
            @Override
            public boolean onMenuItemClick(android.view.MenuItem item) {
                switch (item.getItemId()) {
                    case 1:
                        if (web != null) {
                            web.reload();
                        }
                        return true;
                    case 2:
                        seeded = false;
                        GatewayService.restart(MainActivity.this);
                        toast("正在重启网关服务…");
                        return true;
                    case 3:
                        showLogs();
                        return true;
                    case 4:
                        copyAccessInfo();
                        return true;
                    case 5:
                        copyApiKey();
                        return true;
                    case 6:
                        showSettings();
                        return true;
                    case 7:
                        openBatterySettings();
                        return true;
                    case 8:
                        openPanelInBrowser();
                        return true;
                    case 9:
                        showAbout();
                        return true;
                    case 10:
                        confirmCheckinAll();
                        return true;
                    case 11:
                        confirmRunAllTasks();
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
        new AlertDialog.Builder(this)
                .setTitle(R.string.action_checkin_all)
                .setMessage("将触发全部启用账号签到，并顺带执行猫猫旅行巡检。\n执行进度可在运行日志中查看。")
                .setNegativeButton("取消", null)
                .setPositiveButton("开始", (dialog, which) -> postPanelAction(
                        "/panel/api/checkin_all",
                        "",
                        "全部签到已触发，结果见运行日志"))
                .show();
    }

    private void confirmRunAllTasks() {
        if (!ensurePanelActionReady()) {
            return;
        }
        new AlertDialog.Builder(this)
                .setTitle(R.string.action_run_all_tasks)
                .setMessage("将扫描全部启用账号的成长任务与开学季待办，并开始执行。\n"
                        + "账号内串行、默认并发 1；含真实对话的任务耗时较长。\n"
                        + "执行进度可在任务中心与运行日志中查看。")
                .setNegativeButton("取消", null)
                .setPositiveButton("开始", (dialog, which) -> postPanelAction(
                        "/panel/api/tasks/run_queue",
                        "{\"concurrency\":1,\"growth\":true,\"school\":true}",
                        "任务队列已启动"))
                .show();
    }

    private boolean ensurePanelActionReady() {
        if (!ready) {
            toast("网关尚未就绪，请稍后再试");
            return false;
        }
        if (ServerProcess.readApiKey(this).isEmpty()) {
            toast("API 密钥尚未生成");
            return false;
        }
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
        if (key.isEmpty()) {
            toast("API 密钥尚未生成");
            return;
        }
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
                    connection.setRequestProperty("Authorization", "Bearer " + key);
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
            toast("API 密钥尚未生成");
            return;
        }
        copy("wb2api API Key", key);
    }

    private void openPanelInBrowser() {
        String key = ServerProcess.readApiKey(this);
        if (key.isEmpty()) {
            toast("API 密钥尚未生成");
        } else {
            copy("wb2api API Key", key);
        }
        openExternal(Uri.parse(ServerProcess.panelUrl(this)));
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
            AlertDialog dialog = new AlertDialog.Builder(this)
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
            dialog.show();
            return;
        }

        new AlertDialog.Builder(this)
                .setTitle("已是最新版本")
                .setMessage("当前版本：v" + current + "\n最新版本：" + release.tag)
                .setPositiveButton("访问项目", (d, which) -> openUpstream())
                .setNegativeButton("关闭", null)
                .show();
    }

    private void showUpdateError(String error) {
        new AlertDialog.Builder(this)
                .setTitle("检测更新失败")
                .setMessage(error)
                .setPositiveButton("访问项目", (d, which) -> openUpstream())
                .setNegativeButton("关闭", null)
                .show();
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
        scroller.addView(tv);

        new AlertDialog.Builder(this)
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
                .show();
    }

    private void copyAccessInfo() {
        int port = Prefs.port(this);
        String key = ServerProcess.readApiKey(this);
        String lanIp = LocalIp.get();

        StringBuilder sb = new StringBuilder();
        sb.append("控制台: ").append(ServerProcess.panelUrl(this)).append('\n');
        if (Prefs.lan(this) && lanIp != null) {
            sb.append("局域网 Base URL: http://").append(lanIp).append(':').append(port).append("/v1\n");
        } else {
            sb.append("局域网访问: 已关闭（面板 → 配置里也能改 listen）\n");
        }
        sb.append("API Key: ").append(key.isEmpty() ? "(尚未生成)" : key);
        copy("wb2api 访问信息", sb.toString());
    }

    private void showSettings() {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        int pad = dp(20);
        box.setPadding(pad, dp(8), pad, 0);

        final EditText portInput = new EditText(this);
        portInput.setInputType(InputType.TYPE_CLASS_NUMBER);
        portInput.setHint("监听端口");
        portInput.setText(String.valueOf(Prefs.port(this)));

        final CheckBox lanBox = new CheckBox(this);
        lanBox.setText("允许局域网设备访问（关闭则只监听 127.0.0.1）");
        lanBox.setChecked(Prefs.lan(this));

        final CheckBox bootBox = new CheckBox(this);
        bootBox.setText("开机自动启动网关");
        bootBox.setChecked(Prefs.autoStart(this));

        box.addView(portInput);
        box.addView(lanBox);
        box.addView(bootBox);

        new AlertDialog.Builder(this)
                .setTitle(R.string.action_settings)
                .setView(box)
                .setPositiveButton("保存并重启网关", new android.content.DialogInterface.OnClickListener() {
                    @Override
                    public void onClick(android.content.DialogInterface d, int w) {
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
                        Prefs.setPort(MainActivity.this, port);
                        Prefs.setLan(MainActivity.this, lanBox.isChecked());
                        Prefs.setAutoStart(MainActivity.this, bootBox.isChecked());
                        seeded = false;
                        clearWebCache();
                        GatewayService.restart(MainActivity.this);
                    }
                })
                .setNegativeButton("取消", null)
                .show();
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
        new AlertDialog.Builder(this)
                .setTitle(R.string.action_about)
                .setMessage(text)
                .setPositiveButton(R.string.action_update,
                        (dialog, which) -> checkForUpdates())
                .setNeutralButton(R.string.action_upstream,
                        (dialog, which) -> openUpstream())
                .setNegativeButton("关闭", null)
                .show();
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
