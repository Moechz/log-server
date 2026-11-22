#!/usr/bin/env bash
# ============================================================================
# log-server-for-tos deb 构建（全容器；本机无需 node / go / dpkg 工具链）
#
# 用法：
#   ./build.sh                 # 构建当前架构（宿主 arch）
#   ./build.sh amd64           # x86_64
#   ./build.sh arm64           # aarch64
#   ./build.sh all             # 两个架构都构建
#   ./build.sh info            # 查看配置
#
# 阶段：
#   webui    前端（零构建，直接打包 frontend/ → webui.bz2）
#   backend  后端（golang:1.22，CGO_ENABLED=0 静态二进制）
#   engine   存储引擎（VictoriaLogs，源码可复现配方，见 repro-build.sh）
#   stage    组装 deb 目录树
#   deb      dpkg-deb 打包
#
# 设计依据：TOS-DEB-PACKAGING-GUIDE.md（工具链护栏 = Ubuntu 22.04 = TOS 7 基座）
# ============================================================================
set -euo pipefail
umask 022   # 产物权限固定为 644/755（避免 umask 027 导致 .sha256 出现 640）

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PKG="$ROOT/packaging"
OUT="$ROOT/out"
BUILD="$ROOT/.build"
TPL="$PKG/templates"
STAGE="$BUILD/stage"

APP_ID=log-server-for-tos
VERSION="${VERSION:-0.1.7}"
GO_IMAGE="${GO_IMAGE:-golang:1.22-bookworm}"
NODE_IMAGE="${NODE_IMAGE:-node:22-alpine}"
# 构建基座固定 Ubuntu 22.04（= TOS 7 基座），见打包指南坑 32 配方第 2 条
DEB_IMAGE="${DEB_IMAGE:-ubuntu:22.04}"

log()  { printf '\033[1;34m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*"; }
warn() { printf '\033[1;33m[WARN]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[ERR ]\033[0m %s\n' "$*" >&2; exit 1; }

arch_to_go()    { case "$1" in amd64) echo amd64;; arm64) echo arm64;; *) die "未知架构 $1";; esac; }
arch_to_platf() { case "$1" in amd64) echo x86_64 ;; arm64) echo aarch64 ;; *) die "未知架构 $1" ;; esac; }

# ELF e_machine（小端）：amd64 = 3e00，aarch64 = b700。
# 用途：防止 `.build/bin` 里上一架构的旧产物被原样打进另一架构的 deb
# （真机/门禁都会变成"架构对、二进制跑不起来"，且自测工具不一定能发现）。
elf_machine() { od -An -tx1 -j18 -N2 "$1" 2>/dev/null | tr -d ' \n'; }
assert_arch() {
    local f="$1" want="$2" m
    [ -f "$f" ] || return 0
    m="$(elf_machine "$f")"
    case "$want:$m" in
        amd64:3e00|arm64:b700) return 0 ;;
        *) die "二进制架构不匹配：$f 的 e_machine=$m，期望 $want —— 请先运行 build.sh backend $want" ;;
    esac
}

# 构建模式：auto（默认，优先 docker，回退本地）/ docker / local
MODE="${BUILD_MODE:-auto}"

docker_ok() { command -v docker >/dev/null 2>&1 && timeout 5 docker info >/dev/null 2>&1; }
local_ok()  { command -v go >/dev/null 2>&1 || [ -x /usr/local/go/bin/go ]; }

# resolve_mode 在所有阶段之前调用一次
resolve_mode() {
    case "$MODE" in
        docker) docker_ok || die "指定了 BUILD_MODE=docker 但 docker 不可用";;
        local)  local_ok  || die "指定了 BUILD_MODE=local 但找不到 go";;
        auto)
            if docker_ok; then MODE=docker
            elif local_ok; then MODE=local; warn "docker 不可用 → 使用本地工具链（go + dpkg-deb）"
            else die "既无可用 docker，也无本地 go 工具链"; fi;;
        *) die "未知 BUILD_MODE=$MODE（应为 auto/docker/local）";;
    esac
    log "构建模式: $MODE"
}

# gobin 返回可用的 go 可执行文件
gobin() { command -v go 2>/dev/null || echo /usr/local/go/bin/go; }
GOBIN="$(gobin)"

# ── 阶段：webui（★ 前端是零构建的原生 ES Modules，无需 node）────────────
stage_webui() {
    [ -d "$ROOT/frontend" ] || die "缺少 frontend/"
    log "webui: 打包原生前端（零构建）"
    local tmp="$BUILD/webui"
    rm -rf "$tmp"; mkdir -p "$tmp"
    cp -r "$ROOT/frontend/." "$tmp/"
    # 去掉开发期残留
    find "$tmp" -name '*.map' -delete 2>/dev/null || true
    # ★ 前端 import 门禁（真机事故：页面用了未 import 的 ui.js 组件 → ReferenceError，看起来像空白页）
    if command -v python3 >/dev/null 2>&1; then
        python3 "$ROOT/frontend/check-imports.py" "$tmp" || die "前端 import 检查未通过（页面使用了未导入的 ui.js 组件）"
    else
        warn "无 python3，跳过前端 import 门禁"
    fi
    # ★ 资源版本号（防缓存）：把 __ASSET_VER__ 注入为构建版本 →
    #   index.html 的 css/js 与 app.js 的动态 import 都带 ?v=<version>，
    #   升级后 URL 变化，浏览器/平台不会再拿旧的 ES module 缓存。
    grep -rl '__ASSET_VER__' "$tmp" 2>/dev/null | while read -r f; do
        sed -i "s/__ASSET_VER__/$VERSION/g" "$f"
    done
    # ★ webui.bz2 必须是 tar.bz2，且解压后含可打开的 .html
    ( cd "$tmp" && tar -cjf "$BUILD/webui.bz2" . )
    log "webui: $(du -h "$BUILD/webui.bz2" | cut -f1)"
}

# ── 阶段：backend（纯静态、双架构）──────────────────────────────────────
stage_backend() {
    local arch="$1" goarch
    goarch="$(arch_to_go "$arch")"
    [ -d "$ROOT/backend" ] || { warn "backend/ 尚不存在 —— 跳过（骨架阶段正常）"; return 0; }
    log "backend: 构建 $goarch（$GO_IMAGE，CGO_ENABLED=0）"
    local ldf
    ldf="-X github.com/Moechz/log-server/backend/internal/version.Version=$VERSION"
    ldf="$ldf -X github.com/Moechz/log-server/backend/internal/version.BuildDate=${BUILD_DATE:-19700101-000000}"
    ldf="$ldf -X github.com/Moechz/log-server/backend/internal/version.Commit=${GIT_COMMIT:-unknown}"
    if [ "$MODE" = local ]; then
        ( cd "$ROOT/backend" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" GOFLAGS=-trimpath \
            "$GOBIN" build -trimpath -buildvcs=false -ldflags "$ldf" -o "$BUILD/bin/log-server-for-tos" ./cmd/logserver )
    else
        docker run --rm \
            -v "$ROOT:/src" -w /src/backend \
            -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH="$goarch" \
            -e GOFLAGS=-trimpath \
            "$GO_IMAGE" \
            sh -c "go build -trimpath -buildvcs=false -ldflags '$ldf' -o /src/.build/bin/log-server-for-tos ./cmd/logserver"
    fi
    log "backend: $(du -h "$BUILD/bin/log-server-for-tos" | cut -f1)"
}

# ── 阶段：engine（VictoriaLogs，源码可复现 → 过 V6）─────────────────────
stage_engine() {
    local arch="$1"
    log "engine: VictoriaLogs 源码可复现构建（$arch）"
    if [ "${WITH_ENGINE:-0}" != "1" ]; then
        warn "未启用存储引擎（WITH_ENGINE=0）→ 跳过。开机后应用会如实提示「存储引擎不可用」"
        return 0
    fi
    if [ -x "$PKG/repro-build.sh" ]; then
        BUILD_MODE="$MODE" "$PKG/repro-build.sh" --arch "$arch" --out "$BUILD/bin/victoria-logs"
    else
        warn "repro-build.sh 不可执行 —— 跳过"
    fi
}

# ── 阶段：组装 deb 目录树 ────────────────────────────────────────────────
stage_tree() {
    local arch="$1" platf
    platf="$(arch_to_platf "$arch")"
    log "stage: 组装目录树（platform=$platf）"
    rm -rf "$STAGE"; mkdir -p "$STAGE/DEBIAN" \
        "$STAGE/usr/local/$APP_ID/bin" \
        "$STAGE/usr/local/$APP_ID/init.d" \
        "$STAGE/usr/local/$APP_ID/nginx" \
        "$STAGE/usr/local/$APP_ID/images/icons" \
        "$STAGE/etc/systemd/system" \
        "$STAGE/etc/nginx/conf.d" \
        "$STAGE/usr/share/doc/$APP_ID"

    # 模板 → 安装路径（__VERSION__ / __TOS_PLATFORM__ 注入）
    sed -e "s/__VERSION__/$VERSION/g" -e "s/__TOS_PLATFORM__/$platf/g" \
        "$TPL/config.ini" > "$STAGE/usr/local/$APP_ID/config.ini"
    sed -e "s/__VERSION__/$VERSION/g" "$TPL/log-server-for-tos.lang" \
        > "$STAGE/usr/local/$APP_ID/$APP_ID.lang"
    cp "$TPL/log-server-for-tos.env.example"      "$STAGE/usr/local/$APP_ID/$APP_ID.env.example"
    cp "$TPL/privacy-policy.html"        "$STAGE/usr/local/$APP_ID/privacy-policy.html"
    cp "$TPL/init.d/log-server-for-tos.service"   "$STAGE/usr/local/$APP_ID/init.d/"
    cp "$TPL/nginx/log-server-for-tos.conf"       "$STAGE/usr/local/$APP_ID/nginx/"
    cp "$TPL/images/icons/$APP_ID.svg"   "$STAGE/usr/local/$APP_ID/images/icons/"
    # 平台必需文件检查要求应用根目录存在 <appid>.svg；同时保留 images/icons 供 UI 使用
    cp "$TPL/images/icons/$APP_ID.svg"   "$STAGE/usr/local/$APP_ID/$APP_ID.svg"
    cp "$TPL/PROVENANCE.md"              "$STAGE/usr/local/$APP_ID/" 2>/dev/null || true
    # 许可证（放 /usr/local/<appid>/，不放 /usr/share/doc —— 坑 50：会被 TOS dpkg 剥离）
    cp "$ROOT/LICENSE" "$STAGE/usr/local/$APP_ID/" 2>/dev/null || warn "缺少 LICENSE"
    cp "$ROOT/NOTICE"  "$STAGE/usr/local/$APP_ID/" 2>/dev/null || warn "缺少 NOTICE"
    cp "$ROOT/LICENSE" "$STAGE/usr/share/doc/$APP_ID/copyright" 2>/dev/null || true

    # ★ 双落盘：systemd 与 nginx 以 dpkg 实体文件落 /etc（不依赖 postinst 拷贝）
    cp "$TPL/init.d/log-server-for-tos.service"   "$STAGE/etc/systemd/system/"
    # ★ 辅助服务只以实体文件落 /etc（不进 init.d/，见打包指南坑 11）
    cp "$TPL/log-server-for-tos-vl.service"       "$STAGE/etc/systemd/system/"
    cp "$TPL/nginx/log-server-for-tos.conf"       "$STAGE/etc/nginx/conf.d/"


    # webui.bz2
    [ -f "$BUILD/webui.bz2" ] && cp "$BUILD/webui.bz2" "$STAGE/usr/local/$APP_ID/"
    # 二进制
    [ -f "$BUILD/bin/log-server-for-tos" ]     && cp "$BUILD/bin/log-server-for-tos"     "$STAGE/usr/local/$APP_ID/bin/"
    [ -f "$BUILD/bin/victoria-logs" ] && cp "$BUILD/bin/victoria-logs" "$STAGE/usr/local/$APP_ID/bin/"
    assert_arch "$STAGE/usr/local/$APP_ID/bin/log-server-for-tos" "$arch"
    assert_arch "$STAGE/usr/local/$APP_ID/bin/victoria-logs" "$arch"

    # DEBIAN/*
    local darch; case "$arch" in amd64) darch=amd64;; arm64) darch=arm64;; esac
    local isize; isize=$(du -sk "$STAGE/usr" | cut -f1)
    sed -e "s/__VERSION__/$VERSION/g" -e "s/__DEB_ARCH__/$darch/g" \
        -e "s/__INSTALLED_SIZE__/$isize/g" \
        "$PKG/debian/control" > "$STAGE/DEBIAN/control"
    # ★ TOS 坑：/usr/bin/install 是 TOS 自己的「应用安装器」，不是 coreutils install。
    #   裸用 `install -m` 会报 "flag provided but not defined: -m" → 改用 cp + chmod。
    for f in preinst postinst prerm postrm; do
        cp "$PKG/debian/$f" "$STAGE/DEBIAN/$f"
        chmod 0755 "$STAGE/DEBIAN/$f"
    done
    # ★ 打包指南规则：**配置不走 dpkg conffile** —— 用 `log-server-for-tos.env.example` 模板随包，
    #   由 postinst 首装复制为正式文件（升级不覆盖）。
    #   若写成 DEBIAN/conffiles，dpkg-deb 会报：
    #     "conffile '/usr/local/log-server-for-tos/log-server-for-tos.env' does not appear in package"
    #   （因为包内只有 .example，而 conffile 必须在包内存在。）
    # 故此处**不生成** conffiles。

    # ★ 权限规范化：dpkg-deb 要求 DEBIAN 目录权限 ∈ [0755,0775]；
    #   且本机 umask 可能让 mkdir 产出 0750（实测报 "control directory has bad permissions 750"）。
    find "$STAGE" -type d -exec chmod 0755 {} +
    find "$STAGE" -type f -exec chmod 0644 {} +
    chmod 0755 "$STAGE"/DEBIAN/preinst "$STAGE"/DEBIAN/postinst \
               "$STAGE"/DEBIAN/prerm    "$STAGE"/DEBIAN/postrm
    for b in log-server-for-tos victoria-logs; do
        [ -f "$STAGE/usr/local/$APP_ID/bin/$b" ] && chmod 0755 "$STAGE/usr/local/$APP_ID/bin/$b"
    done
    # init.d 下的单元模板保持 0644（systemd 不要求可执行）

    # ★ 可复现：固定文件时间戳（参考既有项目产物均为 1970-01-01）
    find "$STAGE" -exec touch -h -t 197001010000.00 {} + 2>/dev/null || true
    # 所有权归一见 --root-owner-group（dpkg-deb 参数）
    log "stage: installed-size=${isize}KB"
}

# ── 阶段：dpkg-deb ─────────────────────────────────────────────────────
stage_deb() {
    local arch="$1" darch platf
    case "$arch" in amd64) darch=amd64;; arm64) darch=arm64;; esac
    platf="$(arch_to_platf "$arch")"      # x86_64 / aarch64
    mkdir -p "$OUT"
    # 主产物带版本号，便于人工查找/归档：
    #   <appid>_<version>_<platform>.deb
    # 同时保留平台惯例的无版本号副本 <appid>_<platform>.deb（部分渠道按此约定）。
    local out="$OUT/${APP_ID}_${VERSION}_${platf}.deb"
    local compat="$OUT/${APP_ID}_${platf}.deb"
    log "deb: 打包 $out（兼容副本：$compat）"
    if [ "$MODE" = local ]; then
        # --root-owner-group：无需 root 也能把包内属主写成 root:root
        local rel=".build/stage"
        ( cd "$ROOT" && dpkg-deb --build --root-owner-group "$rel" "$out" >/dev/null )
    else
        docker run --rm -v "$ROOT:/src" -w /src "$DEB_IMAGE" \
            sh -c "dpkg-deb --build --root-owner-group /src/.build/stage '$out' >/dev/null"
    fi
    cp -f "$out" "$compat"
    ( cd "$OUT" \
        && sha256sum "$(basename "$out")"    > "$(basename "$out").sha256" \
        && sha256sum "$(basename "$compat")" > "$(basename "$compat").sha256" \
        && chmod 644 "$(basename "$out").sha256" "$(basename "$compat").sha256" )
    log "deb: $(du -h "$out" | cut -f1)  $(sha256sum "$out" | cut -c1-16)…"
    dpkg-deb -I "$out" 2>/dev/null | head -12 || true
}

build_one() {
    local arch="$1"
    log "=== 构建 $APP_ID $VERSION ($arch) ==="
    resolve_mode
    mkdir -p "$BUILD/bin"
    stage_webui
    stage_backend "$arch"
    stage_engine  "$arch"
    stage_tree    "$arch"
    stage_deb     "$arch"
}

cmd="${1:-$(dpkg --print-architecture 2>/dev/null || echo amd64)}"
case "$cmd" in
    info)
        echo "app_id   = $APP_ID"
        echo "version  = $VERSION"
        echo "root     = $ROOT"
        echo "images   = $GO_IMAGE / $NODE_IMAGE / $DEB_IMAGE"
        echo "archs    = amd64 arm64"
        echo "mode     = ${BUILD_MODE:-auto}（可用 BUILD_MODE=local 强制本地构建）"
        echo "engine   = WITH_ENGINE=${WITH_ENGINE:-0}（=1 时构建 VictoriaLogs）"
        ;;
    amd64|arm64) build_one "$cmd" ;;
    all) build_one amd64; build_one arm64 ;;
    webui|backend|engine) "stage_$cmd" amd64 ;;
    # 注：stage 目标是组装 deb 目录树（stage_tree），deb 是仅打包（stage_deb）。
    # 此前两者都走 "stage_$cmd"，导致 `build.sh stage` 调用不存在的 stage_stage（真 bug，已修）。
    # stage/deb 也必须先重建对应架构的产物：直接组装会把 `.build/bin` 里
    # **上一架构**的旧二进制打进本架构的包（实测：aarch64 二进制进了 x86_64 stage）。
    stage|deb)
        resolve_mode
        mkdir -p "$BUILD/bin"
        stage_webui
        stage_backend amd64
        stage_engine  amd64
        stage_tree    amd64
        [ "$cmd" = "deb" ] && stage_deb amd64
        ;;
    *) die "用法: $0 [amd64|arm64|all|info|webui|backend|engine|stage|deb]" ;;
esac
log "完成。产物在 $OUT"
