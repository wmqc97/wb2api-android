package com.wb2api.gateway;

import java.net.Inet4Address;
import java.net.InetAddress;
import java.net.NetworkInterface;
import java.util.Collections;
import java.util.Enumeration;

/** 取本机非回环 IPv4，用于提示局域网调用地址。 */
final class LocalIp {

    private LocalIp() {
    }

    static String get() {
        try {
            Enumeration<NetworkInterface> ifaces = NetworkInterface.getNetworkInterfaces();
            if (ifaces == null) {
                return null;
            }
            for (NetworkInterface nif : Collections.list(ifaces)) {
                if (!nif.isUp() || nif.isLoopback()) {
                    continue;
                }
                String name = nif.getName() == null ? "" : nif.getName();
                // 跳过蜂窝/隧道接口，只认真实局域网地址
                if (name.startsWith("rmnet") || name.startsWith("tun") || name.startsWith("ppp")) {
                    continue;
                }
                for (InetAddress addr : Collections.list(nif.getInetAddresses())) {
                    if (addr instanceof Inet4Address && !addr.isLoopbackAddress() && !addr.isLinkLocalAddress()) {
                        return addr.getHostAddress();
                    }
                }
            }
        } catch (Exception ignored) {
            // 权限或 ROM 限制，交给调用方回落
        }
        return null;
    }
}
