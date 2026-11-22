/* pages/about.js —— 关于（FR-1208/1209/1210）
 * 要求：降级状态下也必须能打开（权限缺失、采集全失败时它就是诊断入口）。
 * 因此本页不依赖任何接口成功返回 —— 全部有静态兜底。
 */

import { card, kv, table, listItem, notice, todoPill, pill, pageHead, esc } from '../ui.js?v=__ASSET_VER__';
import { t } from '../i18n.js?v=__ASSET_VER__';
import * as api from '../api.js?v=__ASSET_VER__';

const FALLBACK_META = {
  name: 'log server for TOS',
  version: '0.1.0-dev',
  build: '—',
  image: '—（待发布）',
  arch: 'x86_64 / aarch64',
  tosMin: 'TOS 7.x',
  publisher: 'Moechz',
  repo: 'https://github.com/Moechz/log-server',
  dataDir: '/Volume1/@apps/log-server-for-tos/data/',
};

/* 能力依赖说明（透明声明，避免用户以为功能坏了）—— 对应平台需求 R1–R8 + D-008 */
const CAPS = [
  ['R2', '系统日志持久化落点', false, '宿主侧不保留历史（/var/log 在内存、journald 仅内存环形缓冲）：应用停机窗口与首次安装的日志无法回填。本应用的采集与留存不依赖它', '仅影响完整性/回填（不影响采集与存储）'],
  ['R3', '主机审计能力（auditd）', false, '需平台内置审计组件 + 开关', '审计模式（syscall 级证据）'],
  ['R4', '容器日志只读出口', false, '需平台提供目录或 API', '监控其他应用'],
  ['R5', 'TOS 日志中心 / 硬件健康只读接口', false, '待官方提供', '统一视图完整性'],
  ['R7', '应用数据备份/迁移契约', false, 'TOS 的系统备份 / 换机迁移 / 快照不覆盖第三方应用数据（已确认）：日志库与配置需自行备份（应用内提供配置导出/导入）', '数据安全'],
  ['R8', '通知类型注册持久性', false, '本版尚未接入 TOS 消息中心（阶段二计划）；接入时需明确 /etc 下注册文件在系统升级后是否保留', 'TOS 消息中心集成'],
  ['—', 'TOS 账号鉴权（D-008）', false, '官方开发文档索取中', '登录体验'],
];

const FAQ = [
  ['为什么看不到系统日志？',
    '系统日志（journald）的读取权限由 TOS 平台控制，当前版本尚未取得该授权。可先使用「自定义路径采集」采集任意文件日志，以及「接收」功能收集其他设备的日志。'],
  ['为什么用 514/6514 这两个端口？',
    '它们是 syslog 的标准端口（514 UDP/TCP、6514 TLS），用标准端口其他设备无需特殊配置即可上报日志。服务启动时只声明一个 Linux 能力 CAP_NET_BIND_SERVICE 用于绑定低端口，不申请其他权限。若端口已被系统服务占用，可在 /usr/local/log-server-for-tos/log-server-for-tos.env 改为 8000–19999 推荐段的高端口并重启服务，同时把设备端配置指向新端口。'],
  ['应用自己的日志在哪？',
    '三处都能看到：①「监控」页有「应用自身日志（最近）」卡片，可实时查看并一键<b>下载</b>；\n' +
    '② 已<b>落盘</b>在数据目录 <span class="mono">/Volume1/@apps/log-server-for-tos/data/logs/app.log</span>' +
    '（JSON Lines 格式，单文件 10 MB 自动轮转、总量上限 200 MB、保留 7 天；重启不丢，写入前自动脱敏）；\n' +
    '③ 进程级输出（启动失败等）在系统日志里：NAS 上执行 <span class="mono">journalctl -u log-server-for-tos</span>。'],
  ['它占多少资源？',
    '默认「低资源」档位：目标占用 < 300 MB 内存、< 2% CPU。实测参考：TOS 自带的 journald + rsyslog 合计仅约 22 MB 内存、0.03% CPU —— 真正消耗资源的是索引与存储，因此档位与保留策略是主要旋钮。'],
  ['日志存在哪里？可以改吗？',
    '默认 /Volume1/@apps/log-server-for-tos/data/（安装时自动选剩余空间最大的数据卷）。运行时数据只写数据卷，绝不写系统分区。'],
  ['怎么备份？',
    'TOS 的「系统备份 / 换机迁移 / 快照」<b>不包含第三方应用的数据</b>（已确认），请自行备份：①「设置 → 配置备份」一键导出全部配置（采集源 / 转发目标 / 告警规则 / 存储位置，JSON；不含日志与 TLS 私钥），换机或重装后导入即恢复；② 要连日志一起保全时，直接备份数据目录 /Volume1/@apps/log-server-for-tos/data/（建议先停止应用，保证引擎写入一致）。'],
  ['为什么改配置提示需要管理员？',
    'TOS 没有向第三方应用提供「查询当前用户 / 角色」的公开接口。为避免同一台 NAS 上任何能打开本应用的用户都能改配置、改存储位置或导出配置，0.1.7 起默认按<b>最小权限</b>运行（operator：检索、告警确认、暂停采集）。<br>' +
    '管理员操作需解锁：首次启动会在数据目录生成 <span class="mono">config/admin.token</span>（权限 0600），用 NAS 管理员读取后，在「设置 → 管理员权限」粘贴即可；令牌只保存在当前标签页，关闭浏览器即失效。'],
  ['审计日志能留存多久？满足合规要求吗？',
    '**当前版本没有 180 天审计留存**：所有日志统一按全局保留期（默认 14 天）淘汰；按分类分档、留存锁定与哈希链完整性校验是阶段二/三目标，尚未实现。当前定位为「审计就绪（Audit-Ready）」，不宣称合规。'],
];

export function render(root) {
  root.innerHTML = [
    pageHead(t('nav.about'), '功能、版本、开发者与使用指南', '<span class="btn" id="copy-ver">复制版本信息</span>'),

    '<div class="grid wide">',

    /* 应用名 + 简介：名称随界面语言（未翻译则回退英文，与 .lang 口径一致）；不再单独列出「应用名称」行 */
    card({
      span: 'span-all',
      body: `<div class="about-app-name">${esc(t('app.title'))}</div>
        <p class="about-intro">
        一款运行在 TOS 上的<b>统一日志平台</b>：一个应用同时承担四种角色 ——
        <b>审计留存</b>（谁在何时做了什么）、<b>实时监控</b>（出事前先知）、
        <b>日志转发</b>（把日志送往另一台日志服务器）、<b>日志接收</b>（作为日志服务器收其他设备的日志）。
        <br><br>全部数据<b>本地处理、不上传任何内容</b>。
      </p>`,
    }),

    /* 版本 */
    card({
      title: '版本信息',
      body: `<div id="about-meta">${kv([
        ['版本', FALLBACK_META.version],
        ['镜像', FALLBACK_META.image],
        ['架构', FALLBACK_META.arch],
        ['开发者', FALLBACK_META.publisher],
      ])}</div>`,
    }),

    /* 开发者 / 反馈（紧随「版本信息」，宽屏时并排在其右侧）*/
    card({
      title: '开发者 / 反馈',
      body: kv([
        ['开发者', FALLBACK_META.publisher],
        ['仓库', FALLBACK_META.repo],
        ['问题反馈', '通过仓库 Issue 提交'],
        ['文档', '应用内「关于」页即使用指南'],
      ]),
    }),

    /* 能力依赖（透明声明） */
    card({
      title: '能力依赖说明',
      badge: pill('透明声明', 'accent'),
      span: 'span-all',
      body: notice('以下能力由 TOS 平台提供。标记为不可用的功能会在界面上灰显并说明原因，而不是静默失败。' +
        '**系统日志（journald）读取**按其应有的权限前提处理（不在此表内）：应用只读、**不会自我授权**' +
        '（不加系统用户组、不改 ACL、不改 rsyslog/journald 配置）；具备读取权限时自动开始采集。' +
        '另注：<b>R2 只影响「宿主侧有没有历史」</b>——本应用收到日志即落数据卷并自行保留，停机窗口的日志无法回填由 R2 决定。', 'info', 'info') +
        `<div style="margin-top:10px">${table(['编号', '平台能力', '状态', '说明', '影响功能'],
          CAPS.map(c => [
            esc(c[0]), esc(c[1]),
            c[2] ? pill('可用', 'ok') : pill('不可用', 'warn'),
            esc(c[3]), esc(c[4]),
          ]))}</div>`,
    }),

    /* 快速上手 */
    card({
      title: '快速上手（5 步）',
      body: `<ol class="steps">
        <li>在「存储」页确认数据卷与保留策略，并用<b>容量预估器</b>核对预计占用。</li>
        <li>在「接收」页启用监听端口，并按提示在设备端配置发送（UI 会给配置片段）。</li>
        <li>在「规则」页确认内置告警规则，按需调整阈值。</li>
        <li>（可选）在「转发」页添加目标，把日志送往另一台日志服务器。</li>
        <li>回到「总览」查看全局状态；异常项会自动置顶。</li>
      </ol>`,
    }),

    /* 隐私与数据流向 */
    card({
      title: '隐私与数据流向',
      body: `<div class="list">
        ${[
          { level: 'ok', name: '数据仅在本机处理', meta: '除你主动配置的转发目标外，不向任何服务器发送数据' },
          { level: 'ok', name: '凭据本地加密存储', meta: 'TLS 私钥 / SMTP 口令 / Webhook 密钥，权限 0600，不入日志与诊断包' },
          { level: 'ok', name: '诊断包自动脱敏', meta: '导出前掩码凭据与敏感内容' },
          { level: 'ok', name: '审计访问留痕', meta: '查看/导出审计数据的行为本身也会被记录' },
        ].map(x => listItem(x)).join('')}
      </div>`,
    }),

    /* FAQ */
    card({
      title: '常见问题',
      span: 'span-all',
      body: FAQ.map(([q, a]) => `
        <div class="about-module" style="margin-bottom:12px">
          <h4>${esc(q)}</h4>
          <p>${esc(a)}</p>
        </div>`).join(''),
    }),

    /* 开源组件 */
    card({
      title: '第三方开源组件',
      badge: todoPill(),
      body: `<div class="muted" style="font-size:12px">
        正式版本将在此列出全部第三方组件及其许可证（合规要求）。当前骨架的组件清单：
      </div>
      ${table(['组件', '用途', '许可证'], [
        ['VictoriaLogs', '日志存储与索引（随包内置，源码可复现构建）', 'Apache-2.0'],
        ['Go 标准库', '后端运行时', 'BSD-3-Clause'],
      ].map(r => r.map(c => esc(c))))}`,
    }),

    /* 诊断与支持 */
    card({
      title: '诊断与支持',
      body: `<div style="display:flex;gap:8px;flex-wrap:wrap">
          <span class="btn primary" id="btn-diag">生成诊断包</span>
          <span class="btn" id="btn-copy2">复制版本信息</span>
        </div>
        <div class="muted" style="font-size:11px;margin-top:10px">
          诊断包包含配置摘要、健康状态、能力探测结果与最近错误，<b>自动脱敏</b>后下载（FR-1003）。
        </div>`,
    }),

    '</div>',
  ].join('');

  bindActions(root);
}

/* ── 交互 ─────────────────────────────────────────────────────── */
function bindActions(root) {
  const versionText = () =>
    `log server for TOS ${FALLBACK_META.version}\n` +
    `架构: ${FALLBACK_META.arch}\n最低 TOS: ${FALLBACK_META.tosMin}\n` +
    `构建: ${FALLBACK_META.build}\n镜像: ${FALLBACK_META.image}`;

  const copy = async (msg) => {
    try {
      await navigator.clipboard.writeText(versionText());
      toastLocal(root, msg || '版本信息已复制');
    } catch {
      // 非 HTTPS / 无剪贴板权限时的兜底
      const ta = document.createElement('textarea');
      ta.value = versionText();
      document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); toastLocal(root, '版本信息已复制'); }
      catch { toastLocal(root, '复制失败，请手动记录版本号', 'crit'); }
      ta.remove();
    }
  };

  const c1 = root.querySelector('#copy-ver');
  const c2 = root.querySelector('#btn-copy2');
  if (c1) c1.addEventListener('click', () => copy());
  if (c2) c2.addEventListener('click', () => copy());

  const diag = root.querySelector('#btn-diag');
  if (diag) diag.addEventListener('click', async () => {
    // NFR-S6：导出前提示并确认（诊断包已自动脱敏）
    if (!window.confirm('诊断包包含：版本信息、配置摘要、健康状态、能力探测结果与最近错误。\n所有凭据/口令已自动脱敏（FR-1003 / NFR-S6）。\n\n确认生成并下载？')) return;
    diag.classList.add('disabled');
    const r = await api.diagnostics();
    diag.classList.remove('disabled');
    if (!r.ok || !r.data) {
      toastLocal(root, (r.error && r.error.message) || '诊断包生成失败（后端未连接）', 'crit');
      return;
    }
    const blob = new Blob([JSON.stringify(r.data, null, 2)], { type: 'application/json' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'log-server-for-tos-diagnostics-' + new Date().toISOString().replace(/[:.]/g, '-') + '.json';
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 5000);
    toastLocal(root, '诊断包已生成并下载（凭据已脱敏）');
  });

  // 尝试拉取真实版本信息（失败静默保留静态兜底）
  api.about(null).then(r => {
    if (!r.ok || !r.data) return;
    const d = r.data;
    const box = root.querySelector('#about-meta');
    if (!box) return;
    box.innerHTML = kv([
      ['版本', d.version || FALLBACK_META.version],
      ['镜像', d.image || '—'],
      ['架构', d.arch || FALLBACK_META.arch],
      ['开发者', d.publisher || FALLBACK_META.publisher],
    ]);
  });
}

/** 本页自带的轻提示（不依赖全局 toast） */
function toastLocal(root, msg, level = 'ok') {
  const box = root || document.getElementById('content');
  if (!box) return;
  const n = document.createElement('div');
  n.className = 'notice' + (level === 'ok' ? '' : ' ' + level);
  n.style.marginBottom = '10px';
  n.textContent = msg;
  box.prepend(n);
  setTimeout(() => n.remove(), 2600);
}
