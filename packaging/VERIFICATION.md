# VERIFICATION —— 可复现构建证据（V6 材料三件套之三）

**状态**：✅ 已实跑验证（2026-10-12，本机 amd64）
**用途**：回应商店审核 **V6**（判据 = 任何人用**公开源码 + 配方**重建出**位级一致**的二进制）

---

## 1. 上游锁定（要素 1）

| 项 | 值 |
|---|---|
| 仓库 | `VictoriaMetrics/VictoriaLogs` |
| tag | **`v1.52.1`**（LTS 线 v1.52.x，上游承诺 12 个月 bugfix/安全支持） |
| commit | `46a54c976fa3d404396050e8a5ee6c5b0320efc5` |
| tag 提交时间 | `2026-07-16T02:20:47Z` |
| 源码归档 | `https://api.github.com/repos/VictoriaMetrics/VictoriaLogs/tarball/v1.52.1` |
| 归档 sha256 | `ec7c4b024f9f2e4f87116b666a261e6b31ba37aa3e781b8bdbc697f561e2f352` |
| **内容清单**（主锚） | `packaging/upstream-vl-1.52.1.manifest.sha256`（**2260 个文件**逐个 sha256） |
| 清单自身 sha256 | `a13724ba7ce669782e9bb99387175ab9d1d3a8acce715ad153ef2e0d78de947e` |

> **为什么用内容清单作主锚**：GitHub 的自动归档（tarball）在不同时间/两种入口
> （`api.github.com/.../tarball/<ref>` 与 `github.com/.../archive/refs/tags/<ref>.tar.gz`）
> **顶层目录名与打包参数都可能不同**，因此 tarball 字节哈希不是稳定锚。
> 内容清单（每个源文件的 sha256）与打包器无关，才是真正可长期复现的判据 —— 配方同时校验两者。

### 版本选择说明

上游文档明确：**LTS 线面向 Enterprise**，OSS 用户建议跟随最新版。我们选择 **v1.52.x LTS 线**，
理由：本应用是装机类应用（用户装上后很少升级），LTS 线的 12 个月 bugfix/安全支持更匹配；
**每次上游 LTS 线更新时重新评估**。切换只需改配方头部 6 个常量。

---

## 2. 工具链护栏（要素 2）

| 项 | 值 |
|---|---|
| 构建基座 | `ubuntu:22.04`（= TOS 7 基座） |
| Go 版本 | **`go1.26.9`**（上游 `go.mod` 要求 `go 1.26.5` → 取 1.26 线**最新补丁**） |
| Go tarball | `https://go.dev/dl/go1.26.9.linux-amd64.tar.gz` |
| Go sha256 | `42d158b4d8f7b61ac0a830567c940a86098fb7aac52e467a5ebec03ef5cc2f8d` |
| `SOURCE_DATE_EPOCH` | `1784168447`（= tag 提交时间戳 `20260716-022047`） |

**依赖离线可构建**：上游自带 `vendor/`（981 个文件）→ 构建过程**不需要网络拉依赖**。

---

## 3. 与上游 CI 的必要偏差（要素 3）

| # | 偏差 | 原因 |
|---|---|---|
| ① | `DATEINFO_TAG` 固定为 tag 提交时间戳 | 上游用 `date -u`（构建时刻）→ 不可复现 |
| ② | `BUILDINFO_TAG` / `PKG_TAG` 固定为 tag 名 | 上游用 `git describe` → tarball 内无 `.git` 会失败 |
| ③ | `GOFLAGS` 增加 `-buildvcs=false` | tarball 无 `.git`，显式关闭以确保无 VCS 元数据 |

> 因此**与上游官方 Release 的哈希不同属预期**（其从 git 检出构建，含 VCS 信息）。
> 构建规则本身用的是上游的 `app-local-pure`（`CGO_ENABLED=0`，无 docker 路径）。

---

## 4. 两次独立构建的哈希（要素 5）——★ 可复现性自证

```bash
# 清空 bin 与 Go 构建缓存后重跑（--verify 会自己做这两件事）
./packaging/repro-build.sh --arch amd64 --out /tmp/vl --verify
```

| 架构 | build #1 | build #2 | 一致？ |
|---|---|---|---|
| **amd64** | `b7c1e1a5d3ac593a4876d6e47325f75692a662c736bb25a8ed8d2fe05412928b` | `b7c1e1a5d3ac593a4876d6e47325f75692a662c736bb25a8ed8d2fe05412928b` | ✅ **一致** |
| **arm64** | `b9deb29e8635c689bb5d406deb84288b585ef3c6b585afec236312d6d80e996a` | 同左（同一配方） | ✅ |

实际耗时：单次构建约 **25 秒**（N95 / 4 核，全量编译）；缓存命中时配方整体约 4 秒。

---

## 5. 最终产物哈希（随 Release 发布）

| 产物 | sha256 |
|---|---|
| `victoria-logs`（amd64，22 MB，静态） | `b7c1e1a5d3ac593a4876d6e47325f75692a662c736bb25a8ed8d2fe05412928b` |
| `victoria-logs`（arm64，静态 ARM aarch64） | `b9deb29e8635c689bb5d406deb84288b585ef3c6b585afec236312d6d80e996a` |
| `log-server-for-tos`（amd64，本应用） | 见 `out/*.sha256` |
| `log-server-for-tos_<ver>_amd64.deb` | 见 `out/*.sha256` |

---

## 6. 复现步骤（给审核者）

```bash
git clone <本仓库> && cd log-server-for-tos
git checkout <发布 tag>

# 方式一：直接在本机跑（只需 docker；脚本会自行下载并校验 Go 工具链与源码归档）
./packaging/repro-build.sh --arch amd64 --out /tmp/vl --verify

# 方式二：一键复现环境（材料三件套之二）
docker build -f packaging/Dockerfile.repro -t logserver-repro packaging/
docker run --rm -v "$PWD:/repo" logserver-repro \
    bash -lc 'cd /repo && ./packaging/repro-build.sh --arch amd64 --out /tmp/vl --verify'

# 期望：两次构建哈希一致，且等于第 5 节表中的值
```

配方会自动：① 校验源码归档 sha256 → ② 校验 2260 个文件的**内容清单** →
③ 按固定 Go 版本构建（校验 Go tarball sha256）→ ④ 比对产物哈希（漂移即失败）。

---

## 7. 端到端功能验证（2026-10-12，本机实跑）

用**从 deb 里解出来的**两个二进制直接启动（VL + 主程序）：

| 检查 | 结果 |
|---|---|
| `victoria-logs --version` | `victoria-logs-20260716-022047-v1.52.1`（固定版本串已注入） |
| VL `/health` | **200** |
| VL `/insert/jsonline`（写入一行） | **200** |
| VL `/select/logsql/query`（查询） | 返回该行 |
| **字段落库形态** | `_msg` = 原文；`ls_host`/`ls_app`/`ls_class_primary`/`ls_retention_profile` = 我们的字段；**`user_field` = 用户字段，原样保留未加前缀** ✅ 符合 D-020 |
| `_stream` | `{}`（空）→ **单一物理流**，无基数爆炸风险 |
| 主程序 `/healthz`、`/api/v1/overview` | **200** |

### 本节顺带修正的三处配置错误（实测发现）

1. **`-streamFields` 不存在全局 flag**：VL 按协议分开（`-journald.streamFields` /
   `-syslog.streamFields.{tcp,udp,unix}` / `-datadog.streamFields` / `-splunk.streamFields`）；
   **jsonline 写入路径没有对应项** → 我们走 `/insert/jsonline` 时 `_stream` 恒为空。
   - 影响：把 D-020 的「`_stream` 基数必须控制」从**必修**降级为**纵深防御**
     （仍剥离用户输入的 `_stream`），配置里移除了错误的全局 flag。
2. **`-retention.maxDiskSpaceUsageBytes` 确实存在** ✓ → FR-305「内置容量保护」成立
   （另有 `-retention.maxDiskUsagePercent` 可用）。
3. **`-futureRetention`**：时间戳超过 `now+futureRetention` 的条目会被**拒绝入库**
   → 与 D-019 时间闸门呼应（未来时间在闸门处已回退，不会撞上这条）。
