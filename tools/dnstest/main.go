// dnstest 是一个最小化的诊断程序：在目标设备上验证 Go 的 DNS 解析路径。
//
// 用途：确认交叉编译产物在 Android 上究竟走哪条解析路径。
// Android 没有 /etc/resolv.conf，纯 Go 解析器（CGO_ENABLED=0）拿不到 nameserver；
// 走 cgo（bionic getaddrinfo → netd）才能正常解析。
//
//	go build -o dnstest ./main.go
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	host := "copilot.tencent.com"
	if len(os.Args) > 1 {
		host = os.Args[1]
	}

	fmt.Printf("time.Now()      = %s\n", time.Now().Format(time.RFC3339))
	fmt.Printf("time.Local      = %s\n", time.Local.String())
	fmt.Printf("TZ env          = %q\n", os.Getenv("TZ"))
	fmt.Printf("resolv.conf     = %v\n", fileExists("/etc/resolv.conf"))

	start := time.Now()
	ips, err := net.LookupHost(host)
	fmt.Printf("LookupHost(%s) = %v err=%v (in %s)\n", host, ips, err, time.Since(start).Round(time.Millisecond))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
