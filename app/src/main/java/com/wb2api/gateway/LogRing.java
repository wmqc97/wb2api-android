package com.wb2api.gateway;

import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.List;

/** 网关 stdout 的内存环形缓冲，供 UI 的“运行日志”对话框读取（不参与持久化）。 */
final class LogRing {

    private static final int CAPACITY = 600;
    private static final ArrayDeque<String> LINES = new ArrayDeque<>();

    private LogRing() {
    }

    static void add(String line) {
        if (line == null) {
            return;
        }
        synchronized (LINES) {
            LINES.addLast(line);
            while (LINES.size() > CAPACITY) {
                LINES.removeFirst();
            }
        }
    }

    static List<String> snapshot() {
        synchronized (LINES) {
            return new ArrayList<>(LINES);
        }
    }

    /** 取末尾 n 行，用于拼错误提示。 */
    static String tail(int n) {
        synchronized (LINES) {
            StringBuilder sb = new StringBuilder();
            int skip = Math.max(0, LINES.size() - n);
            int i = 0;
            for (String s : LINES) {
                if (i++ < skip) {
                    continue;
                }
                if (sb.length() > 0) {
                    sb.append('\n');
                }
                sb.append(s);
            }
            return sb.toString();
        }
    }

    static void clear() {
        synchronized (LINES) {
            LINES.clear();
        }
    }
}
