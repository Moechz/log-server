/* pages/storage.js —— 存储（真实用量 + 容量预估器走接口，口径与文档附录 G 一致） */
import { card, kpis, progress, table, notice, demoPill, pill, pageHead, esc } from '../ui.js?v=__ASSET_VER__';
import { t } from '../i18n.js?v=__ASSET_VER__';
import * as api from '../api.js?v=__ASSET_VER__';

// ⚠️ 阶段一只有全局保留（引擎 -retentionPeriod）；以下是**规划中的分档**，当前不生效。
const PROFILES = [
  ['ops-14d', '14 天', '普通系统日志', '规划中（不生效）'],
  ['ops-90d', '90 天', '应用 / 容器日志', '规划中（不生效）'],
  ['security-180d', '180 天', '安全与认证事件（审计口径）', '规划中（不生效）'],
  ['hardware-365d', '365 天', '硬件 / 存储事件', '规划中（不生效）'],
  ['self-7d', '7 天', '应用自身日志（已落盘 <DataDir>/logs，单文件 10 MB 自动轮转，总量上限 200 MB）', '已生效（自身日志）'],
];

function fmtBytes(b) {
  if (!b) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = Number(b);
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return v.toFixed(v < 10 && i > 0 ? 1 : 0) + ' ' + u[i];
}

export async function render(root) {
  const r = await api.storageUsage(null);
  const offline = r.offline || !r.ok;
  const u = (!offline && r.data.usage) || {};
  const caps = (!offline && r.data.caps) || {};
  const retention = (!offline && r.data.retention_days) || 14;

  root.innerHTML = [
    pageHead(t('nav.storage'), '保留策略 · 容量水位 · 容量预估（写入前就知道会占多少）',
      offline ? demoPill() : '<span class="pill ok">实时</span>'),

    '<div class="grid wide">',

    card({
      title: '容量水位',
      badge: pill(`${(u.watermark_pct || 0).toFixed(1)}%`, (u.watermark_pct || 0) > 85 ? 'warn' : 'ok'),
      body: `
        ${kpis([
          { v: fmtBytes(u.total_bytes), k: '日志占用' },
          { v: fmtBytes(u.disk_free_bytes), k: '磁盘可用' },
          { v: fmtBytes(u.disk_total_bytes), k: '磁盘总量' },
          { v: retention, k: '全局保留', unit: '天' },
        ])}
        ${progress(u.watermark_pct || 0, (u.watermark_pct || 0) > 95 ? 'crit' : (u.watermark_pct || 0) > 85 ? 'warn' : '')}
        ${notice('引擎侧已启用按磁盘空间自动淘汰（-retention.maxDiskSpaceUsageBytes）；应用层在水位越限时停低优先级采集并告警（FR-305）。', 'info', 'info')}`,
    }),

    card({
      title: '存储引擎能力',
      body: `<dl class="kv">
        <dt>引擎</dt><dd>${esc(caps.engine || '—')}</dd>
        <dt>全文检索</dt><dd>${caps.full_text_search ? '可用' : '不可用'}</dd>
        <dt>正则检索</dt><dd>${caps.regex_search ? '可用' : '不可用'}</dd>
        <dt>原生保留淘汰</dt><dd>${caps.native_ttl ? '可用' : '不可用'}</dd>
        <dt>物化聚合</dt><dd>${caps.native_aggregate ? '可用' : '不可用（总览走应用层计数器）'}</dd>
        <dt>SQL 表达力</dt><dd>${caps.sql ? '完整 SQL' : '不支持（日志专用引擎）'}</dd>
        <dt>按分类保留</dt><dd>${caps.per_profile_retention ? '可用' : `不支持（当前为全局 ${retention} 天）`}</dd>
      </dl>`,
    }),

    card({
      title: '容量预估器（FR-311）',
      body: `
        <div style="display:grid;grid-template-columns:auto 1fr;gap:8px 10px;align-items:center;font-size:var(--font-table)">
          <label for="est-eps">日均速率 (EPS)</label><input id="est-eps" class="field" type="number" min="1" value="300">
          <label for="est-days">保留天数</label><input id="est-days" class="field" type="number" min="1" value="${retention}">
          <label for="est-ratio">压缩比</label><input id="est-ratio" class="field" type="number" min="1" step="0.5" value="10">
        </div>
        <div id="est-out" style="margin-top:12px"></div>
        <div class="muted" style="font-size:11px;margin-top:8px">计算由后端完成（口径与文档附录 G 一致）</div>`,
    }),

    card({
      title: `保留策略（当前：全局 ${retention} 天）`,
      span: 'span-all',
      badge: pill('仅全局保留', 'warn'),
      body: notice(`当前版本**只有全局保留**：所有入库日志统一按引擎 \`-retentionPeriod\`（${retention} 天）淘汰。` +
        '下表 ops / security / hardware 分档为**规划中、不生效**；审计类数据不会因此保留 180 天（阶段二实现后另行告知）。', 'warn', 'warn') +
        table(['档案', '保留期', '适用范围', '状态'], PROFILES.map(x => x.map(esc))) +
        `<div style="margin-top:10px">${notice('保留档案在**入库时固化**：修改策略不影响已入库记录的归属（D-005 硬规则 6）。', 'info', 'info')}</div>`,
    }),

    card({
      title: '存储位置',
      span: 'span-all',
      badge: '<span id="loc-badge" class="pill">读取中…</span>',
      body: `<div id="loc-box"><div class="muted" style="font-size:12px">正在读取存储位置…</div></div>`,
    }),

    '</div>',
  ].join('');

  bindEstimator(root);
  bindStorageLocation(root);
}

/* ── 存储位置（FR-307 / D-023：菜单选择 + 重启生效）─────────────────
 * 只写 /Volume* 数据卷；不写系统分区。选择后写入 storage.json，
 * 重启应用（应用中心 停止→启动）后主进程与引擎服务都会落到新目录。
 */
async function bindStorageLocation(root) {
  const box = root.querySelector('#loc-box');
  const badge = root.querySelector('#loc-badge');
  if (!box) return;

  const r = await api.storageLocation(null);
  if (!r.ok || !r.data) {
    if (badge) badge.className = 'pill warn';
    if (badge) badge.textContent = '不可用';
    box.innerHTML = `<div class="muted" style="font-size:12px">存储位置接口不可用（后端未连接或版本较旧）。</div>`;
    return;
  }
  const d = r.data;
  if (badge) {
    badge.className = 'pill ' + (d.restart_required ? 'warn' : 'ok');
    badge.textContent = d.restart_required ? '待重启生效' : '已生效';
  }

  box.innerHTML = `
    <dl class="kv">
      <dt>当前生效</dt><dd class="mono">${esc(d.engine_in_use || '—')}</dd>
      <dt>可用空间</dt><dd>${fmtBytes(d.disk_free_bytes)} / ${fmtBytes(d.disk_total_bytes)}</dd>
    </dl>
    ${d.restart_required
      ? notice(`已选择新的存储位置 <span class="mono">${esc(d.engine_dir || '')}</span>，<b>重启应用后生效</b>（在 TOS 应用中心对「log server for TOS」执行停止→启动；重启前引擎仍写入原目录）。`, 'warn', 'warn')
      : ''}
    <div style="display:flex;gap:8px;flex-wrap:wrap;margin-top:10px">
      <span class="btn primary" id="loc-change">更改位置</span>
    </div>
    <div class="muted" style="font-size:11px;margin-top:8px">
      仅允许 <span class="mono">/Volume*</span> 数据卷；拒绝系统分区与 tmpfs（FR-308）。已入库的历史数据<b>不会</b>自动迁移。
    </div>
    <div id="loc-browser" style="margin-top:12px" hidden></div>`;

  const browser = box.querySelector('#loc-browser');
  const changeBtn = box.querySelector('#loc-change');

  changeBtn.addEventListener('click', () => {
    browser.hidden = !browser.hidden;
    if (!browser.hidden) browse('');
  });

  async function browse(path) {
    const res = await api.storageBrowse(path);
    if (!res.ok || !res.data) {
      browser.innerHTML = `<div class="notice crit">${esc((res.error && res.error.message) || '无法读取目录')}</div>`;
      return;
    }
    const cur = res.data.path || '';
    const entries = res.data.entries || [];
    browser.innerHTML = `
      <div class="card" style="background:var(--surface2)">
        <div style="display:flex;align-items:center;gap:8px;margin-bottom:10px;flex-wrap:wrap">
          <span class="muted">位置：</span><span class="mono">${esc(cur || '/（数据卷）')}</span>
          ${cur ? `<span class="btn" id="loc-up">上一级</span>` : ''}
          <span class="btn primary" id="loc-pick" style="margin-left:auto">选择此文件夹</span>
        </div>
        <div class="list">
          ${entries.length ? entries.map(e => `
            <div class="list-item" data-path="${esc(e.path)}" style="cursor:pointer">
              <span class="name">${e.is_volume ? '💽 ' : '📁 '}${esc(e.name)}</span>
              <span class="meta mono">${esc(e.path)}</span>
              <span class="right">${e.writable ? pill('可写', 'ok') : pill('只读', '')}
                ${e.free_kb ? `<span class="muted">余 ${fmtBytes(e.free_kb * 1024)}</span>` : ''}</span>
            </div>`).join('')
            : '<div class="empty">该目录下没有子文件夹</div>'}
        </div>
        <div style="display:flex;gap:8px;margin-top:12px;flex-wrap:wrap">
          <input class="field grow" id="loc-input" placeholder="或直接输入绝对路径，如 /Volume1/Public/logs" value="${esc(cur)}">
          <span class="btn primary" id="loc-manual">保存此路径</span>
        </div>
        <div class="muted" style="font-size:11px;margin-top:8px">
          若目录不可写：请在 TOS 文件管理器中把该共享文件夹授予应用用户 <b>log-server-for-tos</b> 写权限后重试。
        </div>
      </div>`;

    browser.querySelectorAll('.list-item[data-path]').forEach(el => {
      el.addEventListener('click', () => browse(el.dataset.path));
    });
    const up = browser.querySelector('#loc-up');
    if (up) up.addEventListener('click', () => browse(res.data.parent || ''));
    browser.querySelector('#loc-pick').addEventListener('click', () => {
      const target = cur || '';
      if (!target) { alert('请先进入一个数据卷或文件夹'); return; }
      confirmSave(target);
    });
    browser.querySelector('#loc-manual').addEventListener('click', () => {
      const v = browser.querySelector('#loc-input').value.trim();
      if (v) confirmSave(v);
    });
  }

  function confirmSave(path) {
    if (!window.confirm(`把存储位置改为：\n\n${path}\n\n保存后需重启应用才能生效，确认？`)) return;
    saveLocation(root, path);
  }
}

async function saveLocation(root, dir) {
  const res = await api.putStorageLocation(dir);
  if (!res.ok) {
    window.alert('保存失败：' + ((res.error && res.error.message) || '未知错误'));
    return;
  }
  render(root);   // 重新渲染以显示「待重启生效」
}

function bindEstimator(root) {
  const eps = root.querySelector('#est-eps');
  const days = root.querySelector('#est-days');
  const ratio = root.querySelector('#est-ratio');
  const out = root.querySelector('#est-out');
  if (!eps || !out) return;

  const run = async () => {
    const res = await api.estimate({
      eps: Number(eps.value) || 300,
      days: Number(days.value) || 180,
      ratio: Number(ratio.value) || 10,
      bytes_per_record: 600,
    });
    if (!res.ok) { out.innerHTML = `<div class="notice crit">预估失败</div>`; return; }
    const d = res.data;
    out.innerHTML = `
      ${kpis([
        { v: fmtBytes(d.stored_bytes), k: '预计占用' },
        { v: (d.records / 1e6).toFixed(1), k: '总条数', unit: 'M' },
        { v: fmtBytes(d.bytes_per_day), k: '压缩后日增量' },
        { v: d.used_pct.toFixed(1), k: '占上限', unit: '%' },
      ])}
      ${progress(d.used_pct, d.level === 'crit' ? 'crit' : d.level === 'warn' ? 'warn' : '')}
      ${d.level === 'crit'
        ? notice('超出容量上限：请缩短保留期、降低采集范围，或提高压缩比。', 'crit', 'warn')
        : d.level === 'warn'
          ? notice('接近容量上限（>85%），建议缩短保留期或调整采集范围。', 'warn', 'warn')
          : notice('容量充足。', 'info', 'check')}`;
  };
  [eps, days, ratio].forEach(i => i.addEventListener('input', run));
  run();
}
