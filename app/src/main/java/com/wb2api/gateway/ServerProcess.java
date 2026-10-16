package com.wb2api.gateway;

import android.content.Context;
import android.content.pm.ApplicationInfo;
import android.util.Log;

import org.json.JSONException;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.util.Map;
import java.util.TimeZone;

/**
 * Go 网关子进程的宿主：定位二进制、注入环境变量、拉起进程、转发日志、健康探测。
 *
 * 这里刻意不复制二进制到私有目录：Android 10（API 29）起禁止应用执行自己数据目录里
 * 落盘的文件，唯一允许 exec 的落盘位置是 {@code nativeLibraryDir}——所以 Go 产物
 * 以 {@code libwb2api.so} 的名义打进 jniLibs，由系统在安装时解压到那里。
 */
final class ServerProcess {

    private static final String TAG = "Wb2ApiServer";

    /** 必须是 jniLibs 里的文件名，系统只会解压 lib/<abi>/ 下的条目。 */
    static final String BIN_NAME = "libwb2api.so";

    /** /healthz 响应体里的 service 字段，用来确认端口上跑的确实是本网关。 */
    static final String SERVICE_NAME = "workbuddy2api";

    private static final long LOG_MAX_BYTES = 2L * 1024 * 1024;

    private static Process process;
    private static Thread stdioThread;
    private static Thread reaperThread;
    private static volatile String exitNote = "";

    /**
     * 进程「代际」计数：每次 start() 自增，reaper 只准写属于自己这一代的 exitNote。
     *
     * 为什么需要：destroy() 发的 SIGTERM 会让旧 reaper 线程立刻从 waitFor() 返回，
     * 而重启路径的动作顺序是 shutdown() → start()。旧 reaper 的写入若晚于新 start()
     * 清空 exitNote，就会把「上一个进程正常退出」的死信息留在字段里，waitReady() 见到
     * 非空 exitNote 就判定本次启动失败——于是每次重启都有概率假报启动失败并触发退避重试。
     * 加代际号后旧 reaper 的写入直接被丢弃，只有真正死于本次启动的进程才写得了 exitNote。
     */
    private static long generation = 0;
    private static long exitGeneration = -1;

    private ServerProcess() {
    }

    /** 服务端的工作目录：config.json / auths / data 全在这里，随卸载一起清掉。 */
    static File workDir(Context c) {
        File d = new File(c.getFilesDir(), "gateway");
        if (!d.isDirectory() && !d.mkdirs()) {
            Log.w(TAG, "无法创建 " + d);
        }
        return d;
    }

    static File configFile(Context c) {
        return new File(workDir(c), "config.json");
    }

    static File logFile(Context c) {
        return new File(new File(workDir(c), "logs"), "gateway.log");
    }

    static String baseUrl(Context c) {
        return "http://127.0.0.1:" + Prefs.port(c);
    }

    static String panelUrl(Context c) {
        return baseUrl(c) + "/panel/";
    }

    static synchronized boolean isAlive() {
        return alive(process);
    }

    /**
     * Process.isAlive()/destroyForcibly() 都是 Java 8 才进 Android（API 26），
     * 而本工程 minSdk 24，所以统一用 exitValue() 的经典判活方式。
     */
    private static boolean alive(Process p) {
        if (p == null) {
            return false;
        }
        try {
            p.exitValue();
            return false;
        } catch (IllegalThreadStateException running) {
            return true;
        }
    }

    static String lastNote() {
        return exitNote;
    }

    /**
     * 试绑一下端口，判断本机还能不能监听它。
     *
     * 注意这里只做「本进程能否 bind」的判断：设备上很可能同时装着别的 WorkBuddy 网关
     * （本项目产物或第三方打包），它同样监听 7863 并同样对外汇报 service=workbuddy2api，
     * 光靠健康探测分不出是谁，只有 bind 才说明端口真归我们。
     */
    static boolean portFree(int port, boolean lan) {
        ServerSocket probe = null;
        try {
            probe = new ServerSocket();
            probe.setReuseAddress(true);
            InetSocketAddress addr = lan
                    ? new InetSocketAddress(port)
                    : new InetSocketAddress(InetAddress.getLoopbackAddress(), port);
            probe.bind(addr, 1);
            return true;
        } catch (Exception e) {
            return false;
        } finally {
            if (probe != null) {
                try {
                    probe.close();
                } catch (IOException ignored) {
                    // 关不掉也无所谓，进程退出时自动回收
                }
            }
        }
    }

    /**
     * 从 preferred 起往上找第一个空闲端口。都占了就原样返回 preferred，
     * 让后续 bind 失败走正常的报错路径。
     */
    static int pickFreePort(int preferred, boolean lan, int tries) {
        for (int i = 0; i < tries; i++) {
            int p = preferred + i;
            if (p > 65535) {
                break;
            }
            if (portFree(p, lan)) {
                return p;
            }
        }
        return preferred;
    }

    /** 解析出可执行的 Go 二进制绝对路径；找不到时抛 IOException 由调用方上报 UI。 */
    private static File binary(Context c) throws IOException {
        ApplicationInfo ai = c.getApplicationInfo();
        File dir = new File(ai.nativeLibraryDir == null ? "" : ai.nativeLibraryDir);
        File bin = new File(dir, BIN_NAME);
        if (!bin.isFile()) {
            throw new IOException("APK 里没有找到 " + BIN_NAME
                    + "（nativeLibraryDir=" + dir.getAbsolutePath() + "，ABI=" + android.os.Build.SUPPORTED_ABIS[0] + "）。"
                    + "请确认构建前执行过 scripts/build-go.ps1 把 Go 产物放进 jniLibs。");
        }
        // 系统解压进来的原生库通常已是 0755，这里再兜一次底。
        try {
            if (!bin.canExecute()) {
                Process chmod = new ProcessBuilder("/system/bin/chmod", "0755", bin.getAbsolutePath()).start();
                chmod.waitFor();
            }
        } catch (Exception e) {
            Log.w(TAG, "chmod 失败（继续尝试执行）", e);
        }
        return bin;
    }

    /** 拉起网关进程；已在运行时是空操作。 */
    static synchronized void start(Context ctx) throws IOException {
        if (isAlive()) {
            return;
        }
        final Context app = ctx.getApplicationContext();
        final File bin = binary(app);
        final File wd = workDir(app);
        final int port = Prefs.port(app);
        final boolean lan = Prefs.lan(app);

        // 让 config.json 里的 listen 与 App 设置保持一致，否则面板配置页会显示旧端口，
        // 用户照着重启还会撞端口。写失败不影响启动：下面的 WB2A_LISTEN 优先级更高。
        syncConfigListen(app, port, lan);

        ProcessBuilder pb = new ProcessBuilder(
                bin.getAbsolutePath(),
                "-config", configFile(app).getAbsolutePath());
        pb.directory(wd);
        pb.redirectErrorStream(true);

        Map<String, String> env = pb.environment();
        // Android 既没有 /etc/localtime 也没有 zoneinfo 目录，Go 只有拿到 TZ 才能把
        // time.Local 摆对；二进制里已通过 time/tzdata 内嵌了时区库。
        env.put("TZ", TimeZone.getDefault().getID());
        // 监听地址以 App 设置为准（环境变量优先级高于 config.json）。
        env.put("WB2A_LISTEN", (lan ? ":" : "127.0.0.1:") + port);
        env.put("HOME", wd.getAbsolutePath());
        env.put("TMPDIR", app.getCacheDir().getAbsolutePath());
        if (env.get("ANDROID_ROOT") == null) {
            env.put("ANDROID_ROOT", "/system");
        }
        if (env.get("ANDROID_DATA") == null) {
            env.put("ANDROID_DATA", "/data");
        }

        // 先占代际再清空：这样晚醒的旧 reaper 会因代际不符而丢弃写入（见 generation 注释）。
        final long myGen = ++generation;
        exitNote = "";
        exitGeneration = myGen;
        Log.i(TAG, "启动 " + bin + " (port=" + port + ", lan=" + lan + ", TZ=" + env.get("TZ") + ")");
        Process p = pb.start();
        process = p;
        startStdioPump(app, p);
        startReaper(p, myGen);
    }

    private static void startStdioPump(final Context app, final Process p) {
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                try (BufferedReader r = new BufferedReader(
                        new InputStreamReader(p.getInputStream(), StandardCharsets.UTF_8))) {
                    String line;
                    while ((line = r.readLine()) != null) {
                        LogRing.add(line);
                        appendLog(app, line);
                    }
                } catch (IOException e) {
                    Log.w(TAG, "读取网关输出结束", e);
                }
            }
        }, "wb2api-stdio");
        t.setDaemon(true);
        t.start();
        stdioThread = t;
    }

    private static void startReaper(final Process p, final long gen) {
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    int code = p.waitFor();
                    if (gen == exitGeneration) {
                        // 只有「当前这一代」进程的死讯才写 exitNote，重启时旧进程的
                        // 迟到死讯一律丢弃，否则 waitReady 会把重启误判成启动失败。
                        exitNote = "网关进程退出，code=" + code
                                + (LogRing.tail(6).isEmpty() ? "" : "\n" + LogRing.tail(6));
                    }
                    Log.i(TAG, "进程退出 code=" + code);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                }
            }
        }, "wb2api-reaper");
        t.setDaemon(true);
        t.start();
        reaperThread = t;
    }

    /**
     * 停掉进程：Process.destroy() 在 Android 上发 SIGTERM，Go 侧据此走 srv.Shutdown
     * 优雅停机（落盘 state.json）；3 秒还没退再补一次强杀。
     */
    static synchronized void destroy() {
        Process p = process;
        process = null;
        if (p == null) {
            return;
        }
        p.destroy();
        for (int i = 0; i < 30 && alive(p); i++) {
            try {
                Thread.sleep(100);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                break;
            }
        }
        if (alive(p)) {
            Log.w(TAG, "优雅停机超时，强制结束");
            p.destroy();
        }
    }

    /** 读 config.json 里的 api_key；首启前文件不存在时返回空串。 */
    static String readApiKey(Context c) {
        try {
            File f = configFile(c);
            if (!f.isFile()) {
                return "";
            }
            JSONObject o = new JSONObject(readText(f));
            String k = o.optString("api_key", "");
            return k == null ? "" : k;
        } catch (Exception e) {
            Log.w(TAG, "读取 api_key 失败", e);
            return "";
        }
    }

    /**
     * 把 api_key 写进 config.json；key 为空串 = 完全关闭鉴权。
     *
     * api_key 是服务端启动时读进内存的（config.json → Load()），所以改完之后必须重启网关
     * 才生效，调用方负责触发重启，这里只负责落盘。
     * 落盘失败会抛 IOException 让调用方提示用户，不静默吞掉——"改了 key 却没生效"是最难查的一类问题。
     */
    static void writeApiKey(Context c, String key) throws IOException, JSONException {
        File f = configFile(c);
        if (!f.isFile()) {
            // 首启前服务端还没生成配置：这里给不存在的配置塞 key 会和 WriteDefault 打架，
            // 交给启动流程先生成配置再说。
            throw new IOException("配置文件尚未生成，请先启动一次网关");
        }
        String trimmed = key == null ? "" : key.trim();
        JSONObject o = new JSONObject(readText(f));
        o.put("api_key", trimmed);
        byte[] raw = o.toString(2).getBytes(StandardCharsets.UTF_8);
        // 同 syncConfigListen：先写临时文件再换名，避免面板/服务端读到半截 JSON。
        File tmp = new File(f.getParentFile(), "config.json.tmp");
        try (OutputStream out = new FileOutputStream(tmp)) {
            out.write(raw);
        }
        if (!tmp.renameTo(f)) {
            try (OutputStream out = new FileOutputStream(f)) {
                out.write(raw);
            }
            //noinspection ResultOfMethodCallIgnored
            tmp.delete();
        }
        Log.i(TAG, "api_key 已更新（len=" + trimmed.length() + "，重启后生效）");
    }

    static void syncConfigListen(Context c, int port, boolean lan) {
        File f = configFile(c);
        if (!f.isFile()) {
            // 首启：服务端会自己写一份含随机 api_key 的推荐配置，端口默认就是 7863。
            return;
        }
        try {
            JSONObject o = new JSONObject(readText(f));
            String want = (lan ? ":" : "127.0.0.1:") + port;
            if (want.equals(o.optString("listen"))) {
                return;
            }
            o.put("listen", want);
            byte[] raw = o.toString(2).getBytes(StandardCharsets.UTF_8);
            File tmp = new File(f.getParentFile(), "config.json.tmp");
            try (OutputStream out = new FileOutputStream(tmp)) {
                out.write(raw);
            }
            // 先写临时文件再换名，避免面板/服务端读到半截 JSON。
            if (!tmp.renameTo(f)) {
                try (OutputStream out = new FileOutputStream(f)) {
                    out.write(raw);
                }
                //noinspection ResultOfMethodCallIgnored
                tmp.delete();
            }
        } catch (Exception e) {
            Log.w(TAG, "同步 config.json listen 失败（不影响启动）", e);
        }
    }

    /** 健康探测结果。ours=true 表示端口上是本网关（靠 service 字段识别）。 */
    static final class Health {
        final boolean ours;
        final int code;
        final String body;

        Health(boolean ours, int code, String body) {
            this.ours = ours;
            this.code = code;
            this.body = body;
        }
    }

    /** 探测 /healthz；连不上返回 null。注意 healthz 在无账号时也会回 503，属正常。 */
    static Health probe(int port, int timeoutMs) {
        HttpURLConnection conn = null;
        try {
            URL url = new URL("http://127.0.0.1:" + port + "/healthz");
            conn = (HttpURLConnection) url.openConnection();
            conn.setConnectTimeout(timeoutMs);
            conn.setReadTimeout(timeoutMs);
            conn.setUseCaches(false);
            int code = conn.getResponseCode();
            InputStream in = (code >= 400) ? conn.getErrorStream() : conn.getInputStream();
            String body = readAll(in);
            boolean ours = false;
            try {
                ours = SERVICE_NAME.equals(new JSONObject(body).optString("service"));
            } catch (Exception ignored) {
                // 端口被别的程序占用时会拿到非本网关的响应
            }
            return new Health(ours, code, body);
        } catch (Exception e) {
            return null;
        } finally {
            if (conn != null) {
                conn.disconnect();
            }
        }
    }

    /**
     * 带自己的 api_key 探 /status，用来确认端口上跑的**确实是我们这一份**网关。
     *
     * /healthz 不够用：设备上若另装了同款网关，它同样汇报 service=workbuddy2api，
     * 而 /healthz 恒无鉴权，分不出是谁；/status 走 withAuth，只有我们的密钥能拿到 200，
     * 别的实例会回 401。
     */
    static boolean probeOwn(int port, String apiKey, int timeoutMs) {
        if (apiKey == null || apiKey.isEmpty()) {
            return false; // 没有密钥就无从确权，交给调用方回退到 /healthz
        }
        HttpURLConnection conn = null;
        try {
            URL url = new URL("http://127.0.0.1:" + port + "/status");
            conn = (HttpURLConnection) url.openConnection();
            conn.setConnectTimeout(timeoutMs);
            conn.setReadTimeout(timeoutMs);
            conn.setUseCaches(false);
            conn.setRequestProperty("Authorization", "Bearer " + apiKey);
            return conn.getResponseCode() == 200;
        } catch (Exception e) {
            return false;
        } finally {
            if (conn != null) {
                conn.disconnect();
            }
        }
    }

    private static void appendLog(Context c, String line) {
        try {
            File f = logFile(c);
            File dir = f.getParentFile();
            if (dir != null && !dir.isDirectory() && !dir.mkdirs()) {
                return;
            }
            if (f.length() > LOG_MAX_BYTES) {
                File old = new File(dir, "gateway.log.1");
                //noinspection ResultOfMethodCallIgnored
                old.delete();
                //noinspection ResultOfMethodCallIgnored
                f.renameTo(old);
            }
            try (OutputStream out = new FileOutputStream(f, true)) {
                out.write((line + "\n").getBytes(StandardCharsets.UTF_8));
            }
        } catch (Exception ignored) {
            // 日志写失败无所谓，内存环形缓冲仍在
        }
    }

    private static String readText(File f) throws IOException {
        try (InputStream in = new FileInputStream(f)) {
            return readAll(in);
        }
    }

    private static String readAll(InputStream in) throws IOException {
        if (in == null) {
            return "";
        }
        java.io.ByteArrayOutputStream bos = new java.io.ByteArrayOutputStream();
        byte[] buf = new byte[8192];
        int n;
        while ((n = in.read(buf)) > 0) {
            bos.write(buf, 0, n);
        }
        return new String(bos.toByteArray(), StandardCharsets.UTF_8);
    }
}
