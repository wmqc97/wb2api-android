// tzdata.go 把 IANA 时区数据库内嵌进二进制。
//
// 为什么需要：Go 的 time 包默认从 /usr/share/zoneinfo 等**系统目录**读取时区，
// 而 Android 既没有这些目录、也没有 /etc/localtime，于是 time.Local 退化成 UTC。
// 本项目的排程（签到 9/21 点、活跃 10 点、保活 22 点）全部按**本地时间**判断，
// 时区错位会让所有定时任务在错误的时刻触发。
//
// 内嵌 tzdata 后，进程设 `TZ=Asia/Shanghai`（Android 端由 App 用设备时区注入）即可
// 正确解析。代价仅约 450 KB（表未压缩，且 Go 按需解压）。
//
// Android 端（android 工程 Wb2ApiServer.java）在启动本进程时注入 TZ。
package main

import (
	// 空白导入：只为把 tzdata 注册进 time 包的查找链。
	_ "time/tzdata"
)
