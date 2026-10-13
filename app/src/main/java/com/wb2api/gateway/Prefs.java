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
}
