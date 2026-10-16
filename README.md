# WorkBuddy 网关（Android 版）

> **上游项目：** [CSSZYF/wb2api](https://github.com/CSSZYF/wb2api)  
> **上游地址：** https://github.com/CSSZYF/wb2api

把参考项目 `wb2api`（Go 编写的 workbuddy2api 网关 + 内嵌 Web 控制台）搬到 Android 上运行。
做法不是重写业务，而是把**同一份 Go 服务端源码交叉编译成 Android 原生可执行文件**，
以 `libwb2api.so` 的名义打进 APK，由前台服务拉起进程，再用 WebView 承载服务端自带的控制台。
所以账号池、积分、任务、代理等逻辑与桌面版**完全一致**，端口/配置文件也通用。

## 运行结构

```
MainActivity（原生首页 + WebView 控制台）
   ├─ 首页：运行状态 / 控制台地址 / 端口配置
   └─ WebView  ──►  http://127.0.0.1:7863/panel/   控制台
        │
GatewayService (前台服务)  ─►  ServerProcess  ──►  libwb2api.so (Go 网关进程)
        │                                             │
   常驻通知 + 监督重启                        config.json / auths / data / logs
                                                  在 App 私有目录下
```

- `GatewayService`：前台服务，通知常驻；监督线程每 15s 探活 `/healthz`，连续失败或进程退出会自动重启（指数退避，最多 5 次），`START_STICKY` 由系统重建。
- `ServerProcess`：定位二进制（`nativeLibraryDir`）、注入环境变量（`TZ` / `WB2A_LISTEN` / `HOME` / `TMPDIR`）、转发日志、健康探测。
- `MainActivity`：默认是原生首页（运行状态、控制台地址、端口配置），右上角图标可切到内嵌 WebView 控制台；
  WebView 打开面板时会自动把 `config.json` 里的 `api_key` 预注入 `localStorage`，免去每次手填密钥。
- `BootReceiver`：可选开机自启。

## 两个必须这么做的坑

1. **必须开 cgo**。`GOOS=android` 时 Go 工具链强制要求外部链接器（`CGO_ENABLED=0` 直接报
   `requires external (cgo) linking`）；而且 Android 没有 `/etc/resolv.conf`，纯 Go 解析器拿不到 DNS，
   只有 cgo 才会走系统的 netd 解析。
2. **必须叫 `.so` 并放进 `jniLibs`**。Android 10 起禁止应用执行自己数据目录里落盘的文件，
   唯一允许 `exec` 的落盘位置是安装时系统解压出来的 `nativeLibraryDir`。
   因此 `app/build.gradle.kts` 里设置了 `jniLibs.useLegacyPackaging = true`（等价 `extractNativeLibs="true"`），
   否则系统只 mmap 不落盘，`nativeLibraryDir` 会是空的。

另外 Android 既没有 `/etc/localtime` 也没有 zoneinfo 目录，所以服务端新增了 `server/cmd/server/tzdata.go`
（`_ "time/tzdata"`）内嵌时区库，App 再通过 `TZ` 环境变量告诉 Go 用哪个时区。

## 环境要求

- JDK 21（本工程用 `sourceCompatibility = 17`，AGP 8.7.3 需要 JDK 17+）
- Android SDK：`compileSdk 35` + `build-tools` + NDK（脚本自动取 `ndk/` 下版本号最大的一个）
- Go 1.22+，默认找 `%TEMP%\gotool\go\bin\go.exe`，找不到时退回 PATH 里的 `go`
- Gradle 由 wrapper 自带（8.11.1），无需本机安装

`local.properties` 里的 `sdk.dir` 指向本机 Android SDK（默认 `F:\Android\Sdk`）。

## 构建

### 1. 编译 Go 网关（产出两个 ABI 的 `libwb2api.so`）

```powershell
pwsh -NoProfile -File scripts\build-go.ps1
# 只编一个 ABI：
# pwsh -NoProfile -File scripts\build-go.ps1 -Abi arm64
```

产物写入 `app\src\main\jniLibs\arm64-v8a\libwb2api.so` 与 `app\src\main\jniLibs\x86_64\libwb2api.so`。
这两个文件在 `.gitignore` 里（体积大且是构建产物），所以**干净克隆后必须先跑这一步**。

### 2. 打包 APK

```powershell
$env:JAVA_HOME = 'C:\Users\10742\Java\jdk-21.0.7+6'
$env:ANDROID_HOME = 'F:\Android\Sdk'
$env:GRADLE_USER_HOME = 'F:\Android\App\wb2api\.gradle-home'   # 见下方说明
.\gradlew.bat --no-daemon assembleDebug
```

产物：`app\build\outputs\apk\debug\app-debug.apk`。

> `GRADLE_USER_HOME` 指向工程内的 `.gradle-home` 是为了规避某些机器上默认缓存目录里
> `native-platform.dll` 加载失败的问题；本机可自由改到别处。

正式签名包先在本机创建不进入版本库的 `keystore.properties`：

```properties
storeFile=F:/path/to/release.keystore
storePassword=...
keyAlias=...
keyPassword=...
```

然后执行：

```powershell
.\gradlew.bat --no-daemon assembleRelease
```

产物：`app\build\outputs\apk\release\app-release.apk`。

### 安装

```powershell
$adb = 'F:\Android\Sdk\platform-tools\adb.exe'
& $adb install -r -g app\build\outputs\apk\debug\app-debug.apk
& $adb shell am start -n com.wb2api.gateway/.MainActivity

# 从电脑访问设备上的网关（可选）
& $adb forward tcp:17863 tcp:7863
# 然后浏览器打开 http://127.0.0.1:17863/panel/
```

只提供 arm64-v8a 与 x86_64 两个 ABI；真机（绝大多数是 arm64）用 `arm64-v8a`，模拟器用 `x86_64`。

## 使用

- 首次启动会自动生成推荐 `config.json`，**默认不开启鉴权**（`api_key` 为空 = 不鉴权）：
  面板与 `/v1/*` 直接打开即可用，浏览器打开面板也不必先粘贴密钥。
  需要鉴权时把 `api_key` 填进 `config.json`（或用环境变量 `WB2A_API_KEY`）。
  ⚠️ 默认同时监听局域网，不开鉴权等于同一 Wi-Fi 下任何人都能用这个网关（含面板里的账号管理）；
  公共网络下建议补一个密钥。
- **导入账号**：面板 → 账号池 → 添加账号，走系统文件选择器选 JSON 文件（需要 `content://` 访问权限，已开启）。支持上游 v1.9.37/v1.9.38 的嵌套 auth/account、单账号、数组、字典及 `realm: "intl"` / `at` / `rt` 等凭证格式。
- **导出账号**：账号池每行的「导出凭证」可保存单账号 JSON，工具栏「导出全部凭证」可导出全部账号；Android 版通过系统“另存为”界面保存，导出文件可直接重新导入。
- **首页**：启动后是原生首页。「管理界面」卡片列出本机 / Wi-Fi 两个控制台地址与 API Key（各带复制按钮）；
  「网关配置」卡片可直接改监听端口，点「保存」后自动重启网关，「恢复」还原成当前生效的端口。
  顶部大卡片显示运行状态，右侧圆形按钮一键启停网关。
- **打开控制台**：首页右上角外链图标（或菜单 → 浏览器打开面板）用**局域网地址**在系统浏览器打开控制台，
  同一 Wi-Fi 下的电脑 / 平板也能直接访问；没开局域网时回落到 `127.0.0.1`。菜单 → 打开控制台（应用内）
  则切到内嵌 WebView，返回键回到首页。
- **面板主题**：面板标题栏的主题按钮在「跟随系统 → 浅色 → 深色」之间循环切换，选择记在浏览器
  localStorage 里；Android 壳不再把网页端的主题锁死在系统设置上（手动选定后不再随系统变）。
- **端口 / 局域网**：菜单（首页右上角齿轮或控制台里的竖排菜单）→ 设置，可改监听端口、是否允许局域网设备访问、开机自启；
  保存后会自动重启网关。开启局域网后，其它设备用 `http://<手机IP>:<端口>/v1` 作为 Base URL。
- **端口冲突自动顺延**：启动前会先试绑配置端口，若被占用（常见于设备上同时装着另一份 WorkBuddy 网关，
  它也监听默认的 7863 并同样汇报 `service=workbuddy2api`）就自动改用下一个空闲端口并记进设置，
  不会静默连到别人的网关上。实际端口以顶部状态栏和「设置」里的值为准。
  就绪判定也做了确权：用 `config.json` 里的 `api_key` 请求带鉴权的 `/status`，
  只有本实例会回 200，别的实例回 401，因此不会把「别人的网关」当成自己启动成功。
- **访问信息**：菜单 → 复制访问地址与密钥，可一键拿到控制台地址（局域网优先）、局域网 Base URL、API Key。
- **运行日志**：菜单 → 查看运行日志（内存缓冲 + 落盘 `logs/gateway.log`，单文件超过 2MB 滚动）。
- **保持常驻**：菜单 → 忽略电池优化，建议国产 ROM 再手动把本应用加进自启/后台白名单。

### 数据目录

```
/data/data/com.wb2api.gateway/files/gateway/
├── config.json     监听地址、api_key、代理等
├── auths/          账号凭证
├── data/           state.json 等运行状态
└── logs/gateway.log
```

与桌面版完全一致：换设备时把桌面版的 `config.json` 和 `auths` 目录拷进上面的目录即可。
卸载 App 会一并清掉这些数据，迁移前先备份。

## 目录说明

```
app/src/main/java/com/wb2api/gateway/   原生层（Service / WebView / 进程宿主 / 设置）
app/src/main/jniLibs/<abi>/libwb2api.so Go 网关产物（构建生成）
app/src/main/res/                       布局、图标、字符串、主题
scripts/build-go.ps1                    Go 交叉编译脚本
server/                                 参考项目服务端源码（可直接升降级）
tools/                                  辅助工具
tools/dnstest/                          DNS 诊断小程序
```

## 已验证

在 `android-35 google_apis x86_64` 模拟器与一台 arm64 真机（Android 17）上实测：

- APK 安装后 app 进程与 `libwb2api.so` 子进程均在运行
- `/healthz` 返回 `{"service":"workbuddy2api", ...}`（无账号时 `healthy=0`，属正常）
- `/panel/` 返回 200，WebView 正常渲染，`api_key` 自动注入成功
- 面板标签页切换、双指缩放、菜单项均正常
- 真机上 7863 已被另一份网关占用，本应用自动落到 7864 并正常工作，两个实例互不影响

真机验证的平台为 Android 17（API 37）；更低版本尚未逐版实测。

## 已知限制

- release 签名参数从项目根目录的 `keystore.properties` 读取；该文件已加入 `.gitignore`，
  未提供时 `assembleRelease` 会输出未签名包。
- 网关仅支持 arm64-v8a / x86_64，其它 ABI 需自行在 `scripts/build-go.ps1` 里扩展。
