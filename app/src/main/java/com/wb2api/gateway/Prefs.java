package com.wb2api.gateway;

import android.content.Context;
import android.content.SharedPreferences;

/** App 侧配置（端口 / 监听范围 / 自启）。真正的网关配置仍在服务端 config.json 里。 */
final class Prefs {

    /** 与服务端 WriteDefault 的默认端口保持一致，首次启动时两边不会打架。 */
    static final int DEFAULT_PORT = 7863;

    private static final String FILE = "wb2api";
    private static final String K_PORT = "port";
    private static final String K_LAN = "lan";
    private static final String K_AUTO_START = "auto_start";
    private static final String K_NOTIFY = "notify";

    private Prefs() {
    }

    private static SharedPreferences of(Context c) {
        return c.getApplicationContext().getSharedPreferences(FILE, Context.MODE_PRIVATE);
    }

    static int port(Context c) {
        int p = of(c).getInt(K_PORT, DEFAULT_PORT);
        return (p >= 1024 && p <= 65535) ? p : DEFAULT_PORT;
    }

    static void setPort(Context c, int p) {
        of(c).edit().putInt(K_PORT, p).apply();
    }

    /** true = 同时监听局域网（其它设备可用手机 IP 调用）；false = 只监听 127.0.0.1。 */
    static boolean lan(Context c) {
        return of(c).getBoolean(K_LAN, true);
    }

    static void setLan(Context c, boolean v) {
        of(c).edit().putBoolean(K_LAN, v).apply();
    }

    static boolean autoStart(Context c) {
        return of(c).getBoolean(K_AUTO_START, true);
    }

    static void setAutoStart(Context c, boolean v) {
        of(c).edit().putBoolean(K_AUTO_START, v).apply();
    }

    /**
     * 是否显示网关的常驻状态通知（缺省 true）。
     *
     * 关掉只是不打扰用户，**不影响**前台服务保活：前台服务被要求必须配一条通知，
     * 关掉开关时改用到低优先级、不可见文案的占位通知维持进程优先级，
     * 否则网关会被系统当普通后台服务回收。
     */
    static boolean notify(Context c) {
        return of(c).getBoolean(K_NOTIFY, true);
    }

    static void setNotify(Context c, boolean v) {
        of(c).edit().putBoolean(K_NOTIFY, v).apply();
    }
}
