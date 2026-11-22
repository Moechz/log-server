# PROVENANCE —— 包内组件溯源

> 放在 `/usr/local/log-server-for-tos/`（**不放 `/usr/share/doc`** —— 打包指南坑 50：该目录会被 TOS dpkg 剥离），
> 用于直接回应审核的「no auditable source provided in the package」措辞。

## 1. 主程序

| 项 | 值 |
|---|---|
| 组件 | log-server-for-tos（本应用） |
| 来源 | 本仓库（公开） |
| 构建 | 本仓库 `packaging/build.sh`，`CGO_ENABLED=0` 静态链接 |
| 配方 | `packaging/build.sh`（阶段 `backend`） |
| 架构 | `x86_64` / `aarch64`（deb 内为 `amd64` / `arm64`） |

## 2. 存储引擎：VictoriaLogs

| 项 | 值 |
|---|---|
| 上游 | `VictoriaMetrics/VictoriaLogs`（Apache-2.0） |
| 版本 | 见 `packaging/repro-build.sh` 的 `UPSTREAM_TAG` |
| 源码归档 sha256 | 见 `UPSTREAM_SHA256`（单一事实源） |
| commit | 见 `UPSTREAM_COMMIT` |
| 构建配方 | `packaging/repro-build.sh` + `packaging/Dockerfile.repro` |
| 可复现性证据 | `packaging/VERIFICATION.md`（两次独立构建位级一致的哈希表） |
| 工具链护栏 | Go `GO_VERSION`（tarball sha256 固定）+ 构建基座 **Ubuntu 22.04**（= TOS 7 基座） |
| **为何自建而非用上游预编译** | 包内携带 ELF 会命中商店审核 **V6**（判据 = 任何人可用公开源码 + 配方重建出**位级一致**的二进制）。本组件为 **Go 项目**，自建成本低，故走「源码可复现」而**不走豁免申请** |

## 3. 前端

原生 HTML/CSS/JS（ES Modules），**零构建**、无第三方 JS 依赖、无 CDN 引用。
产物 = 仓库内 `frontend/` 目录，打包为 `webui.bz2`（tar.bz2）。

## 4. 本包不包含

- 任何运行时（不捆绑 Node/Python/Java）
- 任何 CGO 依赖
- 任何在线安装步骤（安装过程不联网）

## 5. 许可证

- 本应用：见仓库根 `LICENSE`
- VictoriaLogs：Apache-2.0（`NOTICE` 中保留其版权与许可声明）
