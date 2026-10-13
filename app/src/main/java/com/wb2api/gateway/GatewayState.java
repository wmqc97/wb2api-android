package com.wb2api.gateway;

import android.os.Handler;
import android.os.Looper;

import java.util.concurrent.CopyOnWriteArrayList;

/**
 * 网关运行状态的唯一同步点。
 *
 * 生产者是 {@link GatewayService} 的监督线程（后台线程），消费者是 UI（主线程）；
 * 状态字段用 volatile 保证可见性，回调统一 post 到主线程，避免各处自己写 Handler。
 */
final class GatewayState {

    enum Status {
        /** 未运行。 */
        STOPPED,
        /** 进程已拉起或正在重启退避中，尚未通过健康检查。 */
        STARTING,
        /** /healthz 已确认是本网关。 */
        READY,
        /** 启动失败或反复重启失败。 */
        ERROR,
    }

    interface Listener {
        void onStateChanged(Status status, String message);
    }

    private static final Handler MAIN = new Handler(Looper.getMainLooper());
    private static final CopyOnWriteArrayList<Listener> LISTENERS = new CopyOnWriteArrayList<>();

    private static volatile Status status = Status.STOPPED;
    private static volatile String message = "服务未启动";

    private GatewayState() {
    }

    static Status status() {
        return status;
    }

    static String message() {
        return message;
    }

    static void set(final Status next, final String text) {
        if (next == status && text != null && text.equals(message)) {
            return;
        }
        status = next;
        message = text == null ? "" : text;
        MAIN.post(new Runnable() {
            @Override
            public void run() {
                for (Listener l : LISTENERS) {
                    l.onStateChanged(status, message);
                }
            }
        });
    }

    static void addListener(Listener l) {
        LISTENERS.addIfAbsent(l);
        final Listener listener = l;
        MAIN.post(new Runnable() {
            @Override
            public void run() {
                listener.onStateChanged(status, message);
            }
        });
    }

    static void removeListener(Listener l) {
        LISTENERS.remove(l);
    }
}
