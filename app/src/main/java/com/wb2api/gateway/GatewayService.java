package com.wb2api.gateway;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.graphics.drawable.Icon;
import android.os.Build;
import android.os.IBinder;
import android.util.Log;

/**
 * 承载 Go 网关进程的前台服务。
 *
 * 网关是个常驻网络服务，进程一死 API 就断，所以这里做三层保险：
 *   1. 前台服务 + 常驻通知（进程不会被普通后台回收，用户也能随时手动停）；
 *   2. 监督线程：进程异常退出或 /healthz 连续失败都会自动重启；
 *   3. START_STICKY：服务被系统杀掉后由系统重建。
 */
public class GatewayService extends Service {

    static final String ACTION_START = "com.wb2api.gateway.action.START";
    static final String ACTION_RESTART = "com.wb2api.gateway.action.RESTART";
    static final String ACTION_STOP = "com.wb2api.gateway.action.STOP";

    private static final String TAG = "Wb2ApiService";
    private static final String CHANNEL_ID = "gateway_v2";
    private static final String LEGACY_CHANNEL_ID = "gateway";
    private static final int NOTIFICATION_ID = 7863;

    /** /healthz 连续失败到该次数就重启进程（15s 一次，约 1 分钟判定死亡）。 */
    private static final int HEALTH_FAIL_LIMIT = 4;

    /**
     * 稳态下每隔多久补发一次通知（毫秒）。
     *
     * 为什么需要补发：notify() 只在状态**发生变化**时被调用，稳态（READY 且健康）里
     * 再也不会走它。而 setOngoing(true) 的常驻通知在不少国产 ROM 上仍可被划掉，
     * Android 14+ 也允许用户手动划掉前台服务通知——划掉之后没有任何东西再把它贴回来，
     * 于是网关明明在跑，用户却既看不到状态也没有重启/停止入口。
     * 这里按固定间隔重发一次（用 notify 而非 startForeground，避免反复重置前台优先级），
     * 让被划掉的通知自己回来。
     */ 
    private static final long NOTIFY_REPOST_MS = 60_000L;

    /** 单次启动允许等待就绪的时长：首启要解压 tzdata + 建目录，给足余量。 */
    private static final long READY_TIMEOUT_MS = 45_000L;

    /**
     * 进程还活着但迟迟不就绪时，最多再宽限多久（超过 READY_TIMEOUT_MS 之后才启用）。
     *
     * 为什么需要第二段：进程活着但还没监听上端口（首次解压 tzdata、建目录、恢复 state.json
     * 都可能很慢），此时进程本身毫无问题，旧逻辑到 45s 就判"启动失败"并触发退避重试，
     * 用户看到的就是"网关老是启动失败/模型访问异常"，实际只是启动得慢。
     * 这里只在进程**确实还活着**时继续等，进程一死仍按原判失败，不会无限等待。
     */
    private static final long READY_GRACE_MS = 45_000L;

    private volatile boolean running;
    private Thread supervisor;

    static void start(Context c) {
        send(c, ACTION_START);
    }

    static void restart(Context c) {
        send(c, ACTION_RESTART);
    }

    static void stop(Context c) {
        Intent i = new Intent(c, GatewayService.class).setAction(ACTION_STOP);
        // 用 startForegroundService 而非 stopService：要让服务自己把子进程干净收掉。
        startServiceCompat(c, i);
    }

    private static void send(Context c, String action) {
        startServiceCompat(c, new Intent(c, GatewayService.class).setAction(action));
    }

    private static void startServiceCompat(Context c, Intent i) {
        try {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                c.startForegroundService(i);
            } else {
                c.startService(i);
            }
        } catch (Exception e) {
            // Android 12+ 的后台启动限制在极少数场景会拒绝，重试交给用户点通知/进 App。
            Log.w(TAG, "启动前台服务失败", e);
        }
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    @Override
    public void onCreate() {
        super.onCreate();
        createChannel();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        String action = (intent == null || intent.getAction() == null) ? ACTION_START : intent.getAction();
        createChannel();
        // 必须立刻进前台，否则系统在 5 秒内就会以 ANR 处理 startForegroundService。
        startForeground(NOTIFICATION_ID, currentNotification("正在启动网关服务…"));

        if (ACTION_STOP.equals(action)) {
            shutdown();
            stopForegroundCompat();
            stopSelf();
            return START_NOT_STICKY;
        }
        if (ACTION_RESTART.equals(action)) {
            shutdown();
        }
        if (!running) {
            running = true;
            supervisor = new Thread(new Runnable() {
                @Override
                public void run() {
                    supervise();
                }
            }, "gateway-supervisor");
            supervisor.start();
        }
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        shutdown();
        super.onDestroy();
    }

    /** 停监督线程 + 停进程；可重复调用。 */
    private void shutdown() {
        running = false;
        Thread t = supervisor;
        supervisor = null;
        if (t != null) {
            t.interrupt();
            try {
                t.join(3500);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
        }
        ServerProcess.destroy();
    }

    /** 监督线程主体：启动 → 等就绪 → 持续体检 → 异常重启（指数退避，最多 5 次）。 */
    private void supervise() {
        int attempt = 0;
        while (running) {
            try {
                GatewayState.set(GatewayState.Status.STARTING, "正在启动网关服务…");
                notify("正在启动网关服务…", false);
                adoptFreePort();
                ServerProcess.start(this);

                if (waitReady()) {
                    if (!running) {
                        break;
                    }
                    attempt = 0;
                    markReady();
                    int fails = 0;
                    long lastRepost = System.currentTimeMillis();
                    while (running && ServerProcess.isAlive()) {
                        if (!sleep(15_000)) {
                            break;
                        }
                        if (ownInstanceReady(1500)) {
                            fails = 0;
                            if (GatewayState.status() != GatewayState.Status.READY) {
                                markReady();
                            }
                            // 稳态补发：把被划掉的常驻通知贴回来（见 NOTIFY_REPOST_MS）。
                            if (System.currentTimeMillis() - lastRepost >= NOTIFY_REPOST_MS) {
                                lastRepost = System.currentTimeMillis();
                                // 关掉通知时不能什么都不做：此时前台服务持有的是那条
                                // 占位通知，用户划掉它同样不会回来。用 startForeground
                                // 重新贴一次占位通知，保持前台服务合规。
                                if (Prefs.notify(this)) {
                                    notify("网关运行中 · " + ServerProcess.baseUrl(this), true);
                                } else {
                                    startForeground(NOTIFICATION_ID,
                                            currentNotification("网关运行中"));
                                }
                            }
                        } else if (++fails >= HEALTH_FAIL_LIMIT) {
                            GatewayState.set(GatewayState.Status.STARTING, "健康检查连续失败，正在重启网关…");
                            notify("健康检查失败，正在重启…", false);
                            break;
                        }
                    }
                } else if (running) {
                    String note = ServerProcess.lastNote();
                    String msg = note.isEmpty() ? "网关未能启动，请查看运行日志" : ("启动失败：" + firstLine(note));
                    GatewayState.set(GatewayState.Status.ERROR, msg);
                    notify("启动失败", false);
                }
            } catch (Throwable t) {
                Log.w(TAG, "网关监督异常", t);
                GatewayState.set(GatewayState.Status.ERROR, "启动异常：" + t);
            }

            ServerProcess.destroy();
            if (!running) {
                break;
            }
            attempt++;
            if (attempt > 5) {
                GatewayState.set(GatewayState.Status.ERROR, "反复启动失败，已停止重试。下拉菜单里可手动重启。");
                notify("网关已停止", false);
                return;
            }
            long backoff = Math.min(30_000L, 2_000L * attempt);
            GatewayState.set(GatewayState.Status.STARTING, "启动失败，" + (backoff / 1000) + " 秒后重试…");
            if (!sleep(backoff)) {
                break;
            }
        }
        if (!running) {
            GatewayState.set(GatewayState.Status.STOPPED, "服务已停止");
        }
    }

    private boolean waitReady() {
        long deadline = System.currentTimeMillis() + READY_TIMEOUT_MS;
        // 进程仍活着时的绝对上限：首启慢（解压 tzdata / 建目录 / 恢复 state）不该被判死。
        long hardDeadline = deadline + READY_GRACE_MS;
        while (running && System.currentTimeMillis() < deadline) {
            if (!ServerProcess.isAlive()) {
                // 子进程还没起来或已经退出时绝不能认端口上的响应：设备上若另装了同款网关，
                // 它也会监听同一个默认端口并同样汇报 service=workbuddy2api，只看健康探测
                // 会把「别人的网关」当成自己启动成功。
                if (!ServerProcess.lastNote().isEmpty()) {
                    return false; // 进程已经退了，别白等到超时
                }
            } else {
                if (ownInstanceReady(800)) {
                    return true;
                }
            }
            if (!sleep(400)) {
                return false;
            }
        }
        // 超了常规窗口但进程还活着：继续等一段宽限期，只要最终监听上就算启动成功。
        while (running && ServerProcess.isAlive() && System.currentTimeMillis() < hardDeadline) {
            if (ownInstanceReady(800)) {
                return true;
            }
            if (!ServerProcess.isAlive()) {
                break; // 宽限期间进程死了 → 走正常失败路径
            }
            if (!sleep(400)) {
                return false;
            }
        }
        return false;
    }

    /**
     * 端口上跑的到底是不是我们这一份网关。
     *
     * 优先用「配置里的 api_key + /status（带鉴权）」确权：设备上同时装着别的 WorkBuddy
     * 网关时，它同样监听同一端口、同样汇报 service=workbuddy2api，只有 /healthz 是分不出来的。
     * 密钥还没落盘时（首启极早期）才回退到无鉴权的 /healthz。
     */
    private boolean ownInstanceReady(int timeoutMs) {
        int port = Prefs.port(this);
        String key = ServerProcess.readApiKey(this);
        if (!key.isEmpty()) {
            return ServerProcess.probeOwn(port, key, timeoutMs);
        }
        ServerProcess.Health h = ServerProcess.probe(port, timeoutMs);
        return h != null && h.ours;
    }

    /**
     * 启动前确认端口可用；被别的程序（含设备上另一份 WorkBuddy 网关）占了就顺延到下一个空闲端口，
     * 并把它记进设置，免得每次启动都撞一次。
     */
    private void adoptFreePort() {
        if (ServerProcess.isAlive()) {
            return; // 自己的进程还在跑，端口就是我们的，别乱挪
        }
        int want = Prefs.port(this);
        boolean lan = Prefs.lan(this);
        int got = ServerProcess.pickFreePort(want, lan, 20);
        if (got != want) {
            Log.i(TAG, "端口 " + want + " 已被占用，改用 " + got);
            Prefs.setPort(this, got);
            GatewayState.set(GatewayState.Status.STARTING, "端口 " + want + " 被占用，已改用 " + got + "…");
            notify("端口 " + want + " 被占用，改用 " + got, false);
        }
    }

    private void markReady() {
        String url = ServerProcess.baseUrl(this);
        GatewayState.set(GatewayState.Status.READY, "运行中 · " + url);
        notify("网关运行中 · " + url, true);
    }

    /** 睡一觉；被要求停止时返回 false。 */
    private boolean sleep(long ms) {
        try {
            Thread.sleep(ms);
            return running;
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return false;
        }
    }

    private static String firstLine(String s) {
        int nl = s.indexOf('\n');
        return nl < 0 ? s : s.substring(0, nl);
    }

    private void notify(String text, boolean ready) {
        if (!Prefs.notify(this)) {
            return; // 用户关掉了状态通知
        }
        NotificationManager nm = (NotificationManager) getSystemService(Context.NOTIFICATION_SERVICE);
        if (nm != null) {
            nm.notify(NOTIFICATION_ID, buildNotification(text, ready));
        }
    }

    private void createChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) {
            return;
        }
        NotificationManager nm = (NotificationManager) getSystemService(Context.NOTIFICATION_SERVICE);
        if (nm == null) {
            return;
        }
        // 旧通道可能缓存了旧的通知图标；换用新通道并清理旧通道，确保系统重新读取资源。
        if (nm.getNotificationChannel(LEGACY_CHANNEL_ID) != null) {
            nm.deleteNotificationChannel(LEGACY_CHANNEL_ID);
        }
        if (nm.getNotificationChannel(CHANNEL_ID) != null) {
            return;
        }
        NotificationChannel ch = new NotificationChannel(
                CHANNEL_ID,
                getString(R.string.channel_name),
                NotificationManager.IMPORTANCE_LOW);
        ch.setDescription(getString(R.string.channel_desc));
        ch.setShowBadge(false);
        nm.createNotificationChannel(ch);
    }

    /**
     * 进前台用的通知：按用户开关返回完整状态通知，或一条不可见的最小占位通知。
     *
     * 前台服务**必须**带通知（否则 startForeground 直接抛异常、服务起不来），
     * 所以"关闭通知"不能简单地不传——关掉时改传低优先级、无文案、无操作按钮的占位通知，
     * 它在通知栏里几乎不可见，但照样满足系统对前台服务的要求。
     */
    private Notification currentNotification(String text) {
        if (Prefs.notify(this)) {
            return buildNotification(text, false);
        }
        Notification.Builder b = (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O)
                ? new Notification.Builder(this, CHANNEL_ID)
                : new Notification.Builder(this);
        int silentFlags = PendingIntent.FLAG_UPDATE_CURRENT;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            silentFlags |= PendingIntent.FLAG_IMMUTABLE;
        }
        // 空标题空文案 + 最低优先级：通知栏里基本看不到，但系统仍认为我们合规。
        return b.setSmallIcon(R.drawable.ic_launcher_foreground)
                .setLargeIcon(Icon.createWithResource(this, R.mipmap.ic_launcher))
                .setColor(getColor(R.color.ic_launcher_background))
                .setContentTitle("")
                .setContentText("")
                .setOngoing(true)
                .setOnlyAlertOnce(true)
                .setShowWhen(false)
                .setCategory(Notification.CATEGORY_SERVICE)
                .setPriority(Notification.PRIORITY_MIN)
                .setContentIntent(PendingIntent.getActivity(this, 0,
                        new Intent(this, MainActivity.class), silentFlags))
                .build();
    }

    private Notification buildNotification(String text, boolean ready) {
        int flags = PendingIntent.FLAG_UPDATE_CURRENT;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            flags |= PendingIntent.FLAG_IMMUTABLE;
        }

        Intent open = new Intent(this, MainActivity.class)
                .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP | Intent.FLAG_ACTIVITY_NEW_TASK);
        PendingIntent openPi = PendingIntent.getActivity(this, 0, open, flags);

        Intent restart = new Intent(this, GatewayService.class).setAction(ACTION_RESTART);
        PendingIntent restartPi = PendingIntent.getService(this, 1, restart, flags);

        Intent stop = new Intent(this, GatewayService.class).setAction(ACTION_STOP);
        PendingIntent stopPi = PendingIntent.getService(this, 2, stop, flags);

        Notification.Builder b = (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O)
                ? new Notification.Builder(this, CHANNEL_ID)
                : new Notification.Builder(this);
        b.setSmallIcon(R.drawable.ic_launcher_foreground)
                .setLargeIcon(Icon.createWithResource(this, R.mipmap.ic_launcher))
                .setColor(getColor(R.color.ic_launcher_background))
                .setContentTitle(getString(R.string.app_name))
                .setContentText(text)
                .setContentIntent(openPi)
                .setOngoing(true)
                .setOnlyAlertOnce(true)
                .setShowWhen(false)
                .setCategory(Notification.CATEGORY_SERVICE)
                .addAction(new Notification.Action.Builder(R.drawable.ic_refresh, "重启", restartPi).build())
                .addAction(new Notification.Action.Builder(R.drawable.ic_stop, "停止", stopPi).build());
        return b.build();
    }

    private void stopForegroundCompat() {
        stopForeground(STOP_FOREGROUND_REMOVE);
    }
}
