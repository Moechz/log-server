#!/usr/bin/env bash
# ============================================================================
# VictoriaLogs 源码可复现构建配方（V6 合规，打包指南坑 32 六要素）
#
# 为什么自建：包内携带 ELF → 命中 V6（判据 = 任何人用公开源码 + 配方重建出
# **位级一致**的二进制）。走「源码可复现」而非「豁免申请」。
#
# ┌─ 六要素对照 ────────────────────────────────────────────────────────────┐
# │ 1 锁源码     UPSTREAM_TAG / UPSTREAM_COMMIT / UPSTREAM_TARBALL_SHA256   │
# │              + 内容清单（MANIFEST，与打包器无关，主锚）                  │
# │ 2 锁工具链   GO_VERSION / GO_SHA256（go.dev 官方公布值）+ 基座 Ubuntu 22.04│
# │ 3 命令一致   用上游 app-local-pure 规则（= CGO_ENABLED=0 无 docker 路径） │
# │              偏差仅三处，均已记录（见下方 DEVIATIONS）                    │
# │ 4 CGO        CGO_ENABLED=0（纯 Go；VL 无 CGO 依赖）                      │
# │ 5 位级自证   --verify 跑两次独立构建比对 sha256（并清 Go 缓存）           │
# │ 6 材料三件套 本文件 + Dockerfile.repro + VERIFICATION.md                 │
# └─────────────────────────────────────────────────────────────────────────┘
#
# DEVIATIONS（相对上游 CI 的必要偏差，逐条说明）
#   ① DATEINFO_TAG：上游用 `date -u`（构建时刻，不可复现）→ 固定为 **tag 提交时间戳**
#   ② BUILDINFO_TAG / PKG_TAG：上游用 `git describe`（tarball 内无 .git 会失败）
#      → 固定为 tag 名
#   ③ GOFLAGS 增加 `-buildvcs=false`：tarball 无 .git，显式关闭以确保无 VCS 元数据
#   → 因此**与上游官方 Release 的哈希不同属预期**（上游从 git 检出构建，含 VCS 信息）
#
# 用法：
#   ./repro-build.sh --arch amd64 --out /tmp/victoria-logs
#   ./repro-build.sh --arch arm64 --out /tmp/victoria-logs-arm64
#   ./repro-build.sh --arch amd64 --out /tmp/vl --verify     # 两次独立构建自证
#   BUILD_MODE=local ./repro-build.sh --arch amd64 --out ... # 用本机 Go（不拉镜像）
# ============================================================================
set -euo pipefail

# ── 要素 1：锁源码（★ 单一事实源；改版本只需改这一段）────────────────────
UPSTREAM_REPO="VictoriaMetrics/VictoriaLogs"
UPSTREAM_TAG="v1.52.1"                                   # LTS 线 v1.52.x（12 个月 bugfix/安全支持）
UPSTREAM_COMMIT="46a54c976fa3d404396050e8a5ee6c5b0320efc5"
UPSTREAM_TARBALL_SHA256="ec7c4b024f9f2e4f87116b666a261e6b31ba37aa3e781b8bdbc697f561e2f352"
UPSTREAM_MANIFEST="upstream-vl-1.52.1.manifest.sha256"   # 2260 个文件的内容清单
UPSTREAM_MANIFEST_SHA256="a13724ba7ce669782e9bb99387175ab9d1d3a8acce715ad153ef2e0d78de947e"

# ── 要素 2：锁工具链 ──────────────────────────────────────────────────────
# 上游 go.mod 要求 `go 1.26.5` → 取 1.26 线**最新补丁** 1.26.9（含安全修复）
GO_VERSION="1.26.9"
GO_SHA256="42d158b4d8f7b61ac0a830567c940a86098fb7aac52e467a5ebec03ef5cc2f8d"
BUILD_BASE_IMAGE="${BUILD_BASE_IMAGE:-ubuntu:22.04}"     # = TOS 7 基座（工具链护栏）

# ── 固定的版本注入（要素 3 的偏差 ①②）────────────────────────────────────
SOURCE_DATE_EPOCH="1784168447"                           # 2026-07-16T02:20:47Z（tag 提交时间）
DATEINFO_TAG="20260716-022047"
BUILDINFO_TAG="${UPSTREAM_TAG}"
PKG_TAG="${UPSTREAM_TAG}"

# ── 期望产物哈希（要素 5 的对照值；由本配方实测得到）──────────────────────
EXPECTED_amd64="b7c1e1a5d3ac593a4876d6e47325f75692a662c736bb25a8ed8d2fe05412928b"
EXPECTED_arm64="b9deb29e8635c689bb5d406deb84288b585ef3c6b585afec236312d6d80e996a"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CACHE="${VLOGS_CACHE:-$HOME/.cache/logserver-vl}"
MODE="${BUILD_MODE:-docker}"

log()  { printf '\033[1;34m[repro]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[repro][WARN]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[repro][ERR]\033[0m %s\n' "$*" >&2; exit 1; }

ARCH=amd64; OUT=""; SRC=""; VERIFY=0
while [ $# -gt 0 ]; do
    case "$1" in
        --arch)   ARCH="$2"; shift 2;;
        --out)    OUT="$2";  shift 2;;
        --source) SRC="$2";  shift 2;;
        --verify) VERIFY=1;  shift;;
        -h|--help) sed -n '2,40p' "$0"; exit 0;;
        *) die "未知参数 $1";;
    esac
done
case "$ARCH" in amd64|arm64) ;; *) die "--arch 只支持 amd64 / arm64";; esac
[ -n "$OUT" ] || die "必须指定 --out"
mkdir -p "$CACHE" "$(dirname "$OUT")"

# ── 步骤 1：源码归档（下载或复用缓存）+ 校验 ──────────────────────────────
ARCHIVE="$CACHE/VictoriaLogs-$UPSTREAM_TAG.tar.gz"
if [ ! -f "$ARCHIVE" ]; then
    log "下载源码归档 $UPSTREAM_TAG（api.github.com，公开仓库无需 token）"
    curl -fsSL --retry 3 --retry-delay 3 \
        -o "$ARCHIVE.part" \
        "https://api.github.com/repos/$UPSTREAM_REPO/tarball/$UPSTREAM_TAG" \
        || die "下载失败"
    mv "$ARCHIVE.part" "$ARCHIVE"
fi
log "校验源码归档 sha256"
echo "$UPSTREAM_TARBALL_SHA256  $ARCHIVE" | sha256sum -c - \
    || die "源码归档哈希不匹配（要素 1 未通过）—— 注意 GitHub 自动归档可能被重新打包；
若确为上游变更，请更新 UPSTREAM_TARBALL_SHA256 与内容清单后重新评审"
gzip -t "$ARCHIVE" || die "归档损坏（gzip 校验失败）"

# ── 步骤 2：解包 + 内容清单校验（主锚：与打包器无关）──────────────────────
if [ -z "$SRC" ]; then
    SRC="$CACHE/src-$UPSTREAM_TAG"
    rm -rf "$SRC"; mkdir -p "$SRC"
    tar -xzf "$ARCHIVE" -C "$SRC" --strip-components=1
fi
[ -f "$SRC/go.mod" ] || die "源码目录无效：$SRC"

MF="$SCRIPT_DIR/$UPSTREAM_MANIFEST"
if [ -f "$MF" ]; then
    log "校验内容清单（$(wc -l < "$MF") 个文件）"
    echo "$UPSTREAM_MANIFEST_SHA256  $MF" | sha256sum -c - >/dev/null \
        || die "内容清单自身哈希不匹配（清单可能被篡改）"
    ( cd "$SRC" && sha256sum -c "$MF" --quiet ) \
        || die "源码内容与清单不一致（要素 1 主锚未通过）—— 源码被改动或版本不符"
    log "✅ 源码内容与清单逐文件一致"
else
    warn "未找到内容清单 $MF —— 跳过（仅校验归档哈希）"
fi

# ── 步骤 3：构建 ─────────────────────────────────────────────────────────
build_docker() {
    log "容器构建（$BUILD_BASE_IMAGE；Go $GO_VERSION 由配方下载并校验）"
    docker run --rm \
        -v "$SRC:/src-ro:ro" -v "$(cd "$(dirname "$OUT")" && pwd):/out" \
        -v "$CACHE:/cache" \
        -e GO_VERSION="$GO_VERSION" -e GO_SHA256="$GO_SHA256" \
        -e TARGET_ARCH="$ARCH" -e DATEINFO_TAG="$DATEINFO_TAG" \
        -e BUILDINFO_TAG="$BUILDINFO_TAG" -e PKG_TAG="$PKG_TAG" \
        -e OUTFILE="$(basename "$OUT")" \
        "$BUILD_BASE_IMAGE" /bin/bash -euo pipefail -c '
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -qq >/dev/null
            apt-get install -y -qq curl make ca-certificates >/dev/null
            # 工具链护栏：按固定版本下载 + sha256 校验（漂移即拒绝）
            if [ ! -x /cache/go/bin/go ]; then
                curl -fsSL -o /cache/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
                echo "${GO_SHA256}  /cache/go.tgz" | sha256sum -c -
                rm -rf /cache/go && mkdir -p /cache/go
                tar -C /cache/go --strip-components=1 -xzf /cache/go.tgz
            fi
            export PATH=/cache/go/bin:$PATH
            go version
            cp -r /src-ro /src
            cd /src
            # 要素 3：用上游 app-local-pure 规则（CGO_ENABLED=0，无 docker 路径）
            APP_NAME=victoria-logs GOFLAGS="-trimpath -buildvcs=false" \
            CGO_ENABLED=0 GOOS=linux GOARCH="${TARGET_ARCH}" \
            make app-local-goos-goarch \
                DATEINFO_TAG="${DATEINFO_TAG}" BUILDINFO_TAG="${BUILDINFO_TAG}" PKG_TAG="${PKG_TAG}"
            cp "bin/victoria-logs-linux-${TARGET_ARCH}" "/out/${OUTFILE}"
        '
}

build_local() {
    log "本地构建（Go $GO_VERSION 从缓存或 go.dev 获取）"
    local goroot="$CACHE/go"
    if [ ! -x "$goroot/bin/go" ]; then
        log "下载 Go $GO_VERSION（$( [ -f "$CACHE/go.tgz" ] && echo '复用缓存' || echo "$(echo "scale=0; 63" | bc)MB" )）"
        [ -f "$CACHE/go.tgz" ] || curl -fsSL --retry 3 -o "$CACHE/go.tgz" \
            "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
        echo "$GO_SHA256  $CACHE/go.tgz" | sha256sum -c - || die "Go 工具链哈希不匹配"
        rm -rf "$goroot"; mkdir -p "$goroot"
        tar -C "$goroot" --strip-components=1 -xzf "$CACHE/go.tgz"
    fi
    ( export PATH="$goroot/bin:$PATH"
      cd "$SRC"
      APP_NAME=victoria-logs GOFLAGS="-trimpath -buildvcs=false" \
      CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
      make app-local-goos-goarch \
          DATEINFO_TAG="$DATEINFO_TAG" BUILDINFO_TAG="$BUILDINFO_TAG" PKG_TAG="$PKG_TAG" >/dev/null
      cp "bin/victoria-logs-linux-$ARCH" "$OUT" )
}

if [ "$MODE" = local ]; then build_local; else build_docker; fi
[ -f "$OUT" ] || die "构建未产出文件：$OUT"

# ── 步骤 4：产物校验（要素 5）────────────────────────────────────────────
H="$(sha256sum "$OUT" | cut -d' ' -f1)"
EXPECTED="$( eval echo \$EXPECTED_$ARCH )"
log "产物 $OUT"
log "  大小   $(du -h "$OUT" | cut -f1)"
log "  sha256 $H"
if [ -n "$EXPECTED" ] && [ "$H" != "$EXPECTED" ]; then
    warn "与登记值不一致：期望 $EXPECTED"
    warn "  → 若本次为**配方变更**（Go 版本/偏差项调整），请同步更新 EXPECTED_$ARCH 与 VERIFICATION.md"
    [ "${ALLOW_HASH_DRIFT:-0}" = 1 ] || die "哈希漂移被拒绝（设 ALLOW_HASH_DRIFT=1 可放行）"
fi
[ -n "$EXPECTED" ] && [ "$H" = "$EXPECTED" ] && log "✅ 与登记的期望哈希一致"

# ── --verify：两次独立构建自证（清 Go 构建缓存）─────────────────────────
if [ "$VERIFY" = 1 ]; then
    log "--verify：第二次独立构建…"
    rm -rf "$SRC/bin"
    if [ "$MODE" = local ]; then
        rm -rf "${GOCACHE:-$HOME/.cache/go-build}"
        cp "$OUT" "$OUT.build1"
        build_local
        H2="$(sha256sum "$OUT" | cut -d' ' -f1)"
        H1="$(sha256sum "$OUT.build1" | cut -d' ' -f1)"
        rm -f "$OUT.build1"
    else
        cp "$OUT" "$OUT.build1"
        build_docker
        H1="$(sha256sum "$OUT.build1" | cut -d' ' -f1)"; H2="$(sha256sum "$OUT" | cut -d' ' -f1)"
        rm -f "$OUT.build1"
    fi
    log "  build#1 = $H1"
    log "  build#2 = $H2"
    [ "$H1" = "$H2" ] || die "两次构建哈希不一致 —— 不可复现（要素 5 未通过）"
    log "✅ 两次独立构建位级一致（可复现性自证通过）"
fi
