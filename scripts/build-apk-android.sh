#!/bin/sh
# 在设备内的 Ubuntu 容器里打 release APK（不依赖 Windows / Windows 版 NDK）。
#
# 为什么需要这个脚本：Go 网关必须以 cgo 交叉编译成 android/arm64 可执行文件
# （Android 没有 /etc/resolv.conf，纯 Go 解析器拿不到 DNS，只有 cgo 走 bionic/netd
#  才能解析域名），而官方 NDK 只有 x86_64 host 版本 —— 本机是 aarch64，故用
# qemu-x86_64 跑 NDK 的 clang，并绕开 qemu-user 不能 self-exec 的限制：
#   * C 源码   → clang -fintegrated-cc1（cc1 在同进程内跑，不 exec 自己）
#   * 汇编(.S) → 先 -E 预处理，再 clang -cc1as（分两个进程）
#   * 链接     → clang --ld-path 指向包装脚本，由脚本用 qemu 调 ld.lld
#
# 用法：
#   sh scripts/build-apk-android.sh              # 编 Go + 打包 APK
#   sh scripts/build-apk-android.sh --skip-go    # 只打包（复用现有 jniLibs）
#
# 可用环境变量覆盖：ANDROID_SDK / ANDROID_NDK_HOME / WB_KEYSTORE / WB_GRADLE / VERSION
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SDK=${ANDROID_SDK:-/opt/sdk}
NDK=${ANDROID_NDK_HOME:-/root/ndk/android-ndk-r27c}
VERSION=${VERSION:-v1.9.41}
KEYSTORE=${WB_KEYSTORE:-/sdcard/AndroidIDEProjects/keys/wmqc-release.keystore}
BUILD_DIR=${BUILD_DIR:-/tmp/wb2api-apk-build}
TMP=${TMPDIR:-/tmp}
SKIP_GO=0
[ "${1:-}" = "--skip-go" ] && SKIP_GO=1

[ -d "$SDK/platforms" ] || { echo "找不到 Android SDK：$SDK" >&2; exit 1; }

# ── 1) 构建副本 ─────────────────────────────────────────────────────────
# 为什么复制到 /tmp：/sdcard 是 FUSE 挂载，既不支持文件锁（Gradle/Go 都要锁），
# 目录项读取也不稳（tar 会静默解出 0 文件），所以统一用 cp -r 到 /tmp 再构建。
echo "==> 准备构建副本 $BUILD_DIR"
rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR"
cp -r "$ROOT/." "$BUILD_DIR/"
rm -rf "$BUILD_DIR/_gitmeta" "$BUILD_DIR/dist" "$BUILD_DIR/build" \
       "$BUILD_DIR/app/build" "$BUILD_DIR/.gradle" "$BUILD_DIR/local.properties"

# ── 2) Go 交叉编译（android/arm64, cgo）——必须在 Gradle 之前 ────────────
if [ "$SKIP_GO" = 0 ]; then
  [ -d "$NDK/toolchains/llvm/prebuilt/linux-x86_64" ] || { echo "找不到 NDK：$NDK" >&2; exit 1; }
  command -v qemu-x86_64-static >/dev/null || { echo "缺少 qemu-x86_64-static（apt install qemu-user-static）" >&2; exit 1; }

  CC_WRAP=$TMP/cc-android-qemu.sh
  LD_WRAP=$TMP/ld-android-qemu.sh

  cat > "$LD_WRAP" <<'LDEOF'
#!/bin/sh
# Android 15+/16KB 页大小设备要求 PT_LOAD 按 0x4000 对齐；NDK r27 默认仍按
# 0x1000 链接 Go 产物，系统启动时会弹「Android 应用兼容性 / 16KB 不兼容」提示。
# Go 外部链接命令会把 -z max-page-size=0x1000 放在参数尾部，故先重写这两个
# 选项（保持其它 -z 不变），再交给 qemu 下的 ld.lld。
__LD_ARGS__=""
__LD_SKIP__=0
for __LD_A__ in "$@"; do
  if [ "$__LD_SKIP__" = 1 ]; then
    __LD_SKIP__=0
    case "$__LD_A__" in
      max-page-size=*|common-page-size=*) continue;;
    esac
    __LD_ARGS__="$__LD_ARGS__ -z $__LD_A__"
    continue
  fi
  case "$__LD_A__" in
    -z) __LD_SKIP__=1; continue;;
    -Wl,-z,max-page-size=*|-Wl,-z,common-page-size=*) continue;;
  esac
  __LD_ARGS__="$__LD_ARGS__ $__LD_A__"
done
# shellcheck disable=SC2086
exec /usr/bin/qemu-x86_64-static "__TC__/bin/ld.lld" $__LD_ARGS__ \
  -z max-page-size=16384 -z common-page-size=16384
LDEOF

  cat > "$CC_WRAP" <<'CCEOF'
#!/bin/sh
# qemu 下跑 x86_64 版 NDK clang（本机是 aarch64），细节见 build-apk-android.sh 顶部。
TC="__TC__"
Q=/usr/bin/qemu-x86_64-static
TARGET=aarch64-linux-android21
SYSROOT="$TC/sysroot"
src=""; out=""; asm=0; incflags=""; prev=""
CC_ARGS=""; count=0
for a in "$@"; do
  case "$prev" in
    -o) out="$a" ;;
    -I|-D|-U|-include|-isystem|-idirafter) incflags="$incflags $prev $a" ;;
  esac
  case "$a" in
    *.S|*.s) src="$a"; asm=1 ;;
    -Wl,-z,max-page-size=*|-Wl,-z,common-page-size=*) ;; # 丢弃，末尾统一 16KB
    -I*|-D*|-U*|-isystem*) incflags="$incflags $a" ;;
  esac
  prev="$a"
done
if [ "$asm" = 1 ] && [ -n "$src" ] && [ -n "$out" ]; then
  tmp="$out.i"
  $Q "$TC/bin/clang" -fintegrated-cc1 -E --target=$TARGET --sysroot="$SYSROOT" $incflags -o "$tmp" "$src" || exit 1
  $Q "$TC/bin/clang" -cc1as -triple $TARGET -filetype obj -o "$out" "$tmp" || exit 1
  rm -f "$tmp"
  exit 0
fi
# 原样保留全部位置参数（含带空格的路径），只在末尾追加 16KB 链接选项。
set -- "$@" -Wl,-z,max-page-size=16384 -Wl,-z,common-page-size=16384
exec $Q "$TC/bin/clang" -fintegrated-cc1 -Wno-unused-command-line-argument \
  --ld-path=__LDWRAP__ \
  --target=$TARGET --sysroot="$SYSROOT" "$@"
CCEOF

  sed -i "s|__TC__|$NDK/toolchains/llvm/prebuilt/linux-x86_64|g; s|__LDWRAP__|$LD_WRAP|g" "$CC_WRAP" "$LD_WRAP"
  chmod +x "$CC_WRAP" "$LD_WRAP"

  echo "==> 编译 Go（android/arm64, cgo；独立 GOCACHE 保证 16KB 对齐生效）"
  GO_CACHE="$BUILD_DIR/.gocache"
  rm -rf "$GO_CACHE"
  mkdir -p "$GO_CACHE"
  ( cd "$BUILD_DIR/server" && CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC="$CC_WRAP" \
      GOCACHE="$GO_CACHE" \
      go build -trimpath -buildvcs=false -ldflags "-s -w -X main.appVersion=$VERSION" \
      -o "$BUILD_DIR/app/src/main/jniLibs/arm64-v8a/libwb2api.so" ./cmd/server )
  # 回写项目源码目录：下次 --skip-go 打包、以及 IDE 里的产物都指向最新二进制。
  mkdir -p "$ROOT/app/src/main/jniLibs/arm64-v8a"
  cp "$BUILD_DIR/app/src/main/jniLibs/arm64-v8a/libwb2api.so" \
     "$ROOT/app/src/main/jniLibs/arm64-v8a/libwb2api.so"
  echo "    ok: $(wc -c < "$BUILD_DIR/app/src/main/jniLibs/arm64-v8a/libwb2api.so") bytes"
  # 注意：Go 的外部链接缓存按 build ID 复用，某些增量场景会跳过新包装器。
  # 真正上线包以 dist/ 的 ELF 校验为准；下面给出提示，不阻断 --skip-go 流程。
  if command -v readelf >/dev/null 2>&1; then
    ALIGN=$(readelf -lW "$BUILD_DIR/app/src/main/jniLibs/arm64-v8a/libwb2api.so" \
      | awk '$1 == "LOAD" {print $NF}' | sort -u)
    echo "    PT_LOAD 页对齐: $ALIGN（16KB 设备需要 0x4000）"
  fi
fi

# ── 3) Gradle 打包 ──────────────────────────────────────────────────────
printf 'sdk.dir=%s\n' "$SDK" > "$BUILD_DIR/local.properties"
# 签名口令只从环境变量读取，绝不写进仓库；本机可先 export 后再构建：
#   WB_KEYSTORE_PASSWORD=... WB_KEY_PASSWORD=... WB_KEYSTORE_ALIAS=wmqc_release
# 未给口令时不写 keystore.properties（Gradle 产未签名包，避免误把本机口令提交）。
KS_PASS=${WB_KEYSTORE_PASSWORD:-}
KS_ALIAS=${WB_KEYSTORE_ALIAS:-wmqc_release}
KEY_PASS=${WB_KEY_PASSWORD:-$KS_PASS}
if [ -f "$KEYSTORE" ] && [ -n "$KS_PASS" ] && [ -n "$KEY_PASS" ]; then
  printf 'storeFile=%s\nstorePassword=%s\nkeyAlias=%s\nkeyPassword=%s\n' \
    "$KEYSTORE" "$KS_PASS" "$KS_ALIAS" "$KEY_PASS" > "$BUILD_DIR/keystore.properties"
else
  echo "!! 未提供 keystore 或 WB_KEYSTORE_PASSWORD/WB_KEY_PASSWORD —— 将产出未签名 APK" >&2
  rm -f "$BUILD_DIR/keystore.properties"
fi

GRADLE=${WB_GRADLE:-}
if [ -z "$GRADLE" ]; then
  for g in "$HOME"/.gradle/wrapper/dists/gradle-8.9-bin/*/gradle-8.9/bin/gradle \
           "$HOME"/.gradle/wrapper/dists/gradle-8.*-bin/*/gradle-8.*/bin/gradle; do
    [ -x "$g" ] && GRADLE=$g && break
  done
fi
[ -n "$GRADLE" ] || GRADLE=$(command -v gradle || true)
[ -n "$GRADLE" ] || { echo "找不到 Gradle（8.9+）" >&2; exit 1; }

echo "==> Gradle: $GRADLE"
( cd "$BUILD_DIR" && "$GRADLE" --no-daemon -q :app:assembleRelease )

# ── 4) 产物 ─────────────────────────────────────────────────────────────
mkdir -p "$ROOT/dist"
OUT="$ROOT/dist/wb2api-${VERSION#v}-arm64-release.apk"
cp "$BUILD_DIR/app/build/outputs/apk/release/app-release.apk" "$OUT"
echo "==> 完成：$OUT"
ls -l "$OUT"
