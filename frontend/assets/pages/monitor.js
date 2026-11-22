/* pages/monitor.js —— 监控（真实告警来自 GET /api/v1/alerts）
 * FR-804：告警确认 / 静默 / 维护窗口（把观察面变成可操作台）。
 * FR-807：告警一键跳到触发它的日志上下文。FR-404：告警导出。
 */
import { card, kpis, table, logPanel, notice, demoPill, pill, dot, pageHead, esc } from '../ui.js?v=__ASSET_VER__';
import { t } from '../i18n.js?v=__ASSET_VER__';
import * as api from '../api.js?v=__ASSET_VER__';

export async function render(root, ctx) {
  const [ar, sr] = await Promise.all([api.alerts(100, null), api.selflog(50, null)]);
  const offline = ar.offline || !ar.ok;
  const stats = (!offline && ar.data.stats) || { active: 0, by_level: {}, total: 0 };
  const items = (!offline && ar.data.items) || [];
  const silences = (!offline && ar.data.silences) || [];
  const selfItems = (!sr.offline && sr.ok && sr.data.items) || [];
  const selfFile = (!sr.offline && sr.ok && sr.data.file) || {};
  const selfStats = (!sr.offline && sr.ok && sr.data.stats) || {};
  const mb = (b) => (Number(b || 0) / 1048576).toFixed(1) + ' MB';
  const maint = !!stats.maintenance;

  root.innerHTML = [
    pageHead(t('nav.monitor'), '实时告警与安全事件：先看有没有事，再看趋势', offline ? demoPill() : '<span class="pill ok">实时</span>'),

    maint ? notice(`维护窗口进行中：告警已静默（不会产生新告警、不会通知）。可在下方「静默与维护窗口」结束。`, 'warn', 'warn') : '',

    '<div class="grid wide">',

    card({
      title: '活跃告警',
      badge: pill(`${stats.active} 条` + (stats.acked ? `（${stats.acked} 已确认）` : ''), stats.active > 0 ? 'crit' : 'ok'),
      cls: stats.active > 0 ? 'crit-border' : '',
      span: 'span-all',
      body: (items.length ? `
          <div style="display:flex;gap:8px;margin-bottom:10px;flex-wrap:wrap">
            <span class="btn" id="ack-all">确认全部</span>
            <span class="btn ${maint ? 'primary' : ''}" id="maint">${maint ? '结束维护窗口' : '进入维护窗口'}</span>
            <span class="btn" id="alerts-json">导出 JSON</span>
            <span class="btn" id="alerts-csv">导出 CSV</span>
            <span class="muted" style="font-size:11px;align-self:center">「静默」后该规则/主机的告警在设定时间内不再产生与通知</span>
          </div>
          <div class="list">${items.map((a, i) => alertRow(a, i)).join('')}</div>`
        : `<div class="empty">当前无活跃告警</div>`),
    }),

    card({
      title: '告警统计',
      body: `${kpis([
        { v: stats.active || 0, k: '活跃' },
        { v: stats.acked || 0, k: '已确认' },
        { v: (stats.by_level && stats.by_level['严重']) || 0, k: '严重' },
        { v: (stats.by_level && stats.by_level['警告']) || 0, k: '警告' },
        { v: stats.total || 0, k: '累计触发' },
      ])}`,
    }),

    card({
      title: '命中规则 TOP',
      body: Object.keys(stats.rule_hits || {}).length
        ? table(['规则', '命中次数'], Object.entries(stats.rule_hits).sort((a, b) => b[1] - a[1]).slice(0, 10)
            .map(([k, v]) => [esc(k), esc(v)]))
        : `<div class="empty">暂无命中</div>`,
    }),

    card({
      title: '静默与维护窗口',
      span: 'span-all',
      badge: pill(`${silences.length} 条`, silences.length ? 'warn' : ''),
      body: silences.length
        ? `<div class="list">${silences.map((s, i) => silenceRow(s, i)).join('')}</div>`
        : `<div class="empty">当前没有静默 / 维护窗口</div>`,
    }),

    card({
      title: '应用自身日志（最近）',
      span: 'span-all',
      badge: selfFile.path ? pill(selfFile.files + ' 个文件') : pill('self log'),
      body: `
        <div class="muted" style="font-size:12px;margin-bottom:10px">
          ${
            selfFile.path
              ? `已<b>落盘</b>：<span class="mono">${esc(selfFile.path)}</span>（共 ${esc(selfFile.files)} 个文件 / ${mb(selfFile.bytes)}，
                 上限 单文件 ${mb(selfFile.max_file_bytes)}、总量 ${mb(selfFile.max_total_bytes)}、保留 ${esc(selfFile.max_age_days)} 天；重启不丢）
                 <span class="btn" id="selflog-dl" style="margin-left:8px">下载最近日志</span>`
              : '本次仅<b>内存</b>保留（落盘未启用或数据目录不可写）；进程输出可在 NAS 上执行 <span class="mono">journalctl -u log-server-for-tos</span> 查看。'
          }
          <br>内存环形缓冲：${esc(selfStats.used || 0)}/${esc(selfStats.capacity || 0)} 条（用于实时展示，重启清空）。
        </div>`
        + (selfItems.length
          ? logPanel(selfItems.slice(0, 30).map(e => ({
              ts: (e.ts || '').slice(11, 19), level: e.level, text: `[${e.module}] ${e.msg}`,
            })))
          : `<div class="empty">暂无</div>`),
    }),

    '</div>',
  ].join('');

  /* ── 交互 ─────────────────────────────────────────────────────── */
  const reload = () => render(root, ctx);

  // FR-807：告警 → 日志上下文
  root.querySelectorAll('[data-ctx]').forEach((b) => {
    b.addEventListener('click', () => {
      const a = items[Number(b.dataset.ctx)];
      if (!a) return;
      const p = { pivot: a.at };
      if (a.sample_raw) p.q = a.sample_raw.slice(0, 120);
      if (a.host) p.host = a.host;
      if (ctx && typeof ctx.navigate === 'function') ctx.navigate('search', p);
    });
  });

  // FR-804：确认
  root.querySelectorAll('[data-ack]').forEach((b) => b.addEventListener('click', async () => {
    const a = items[Number(b.dataset.ack)];
    if (!a) return;
    b.textContent = '…';
    const r = await api.alertAck(a.id);
    if (!r.ok) window.alert('确认失败：' + ((r.error && r.error.message) || ''));
    reload();
  }));
  const ackAll = root.querySelector('#ack-all');
  if (ackAll) ackAll.addEventListener('click', async () => {
    if (!window.confirm('确认全部活跃告警？')) return;
    const r = await api.alertAckAll();
    if (!r.ok) window.alert('操作失败：' + ((r.error && r.error.message) || ''));
    reload();
  });

  // FR-804：静默（按规则 + 主机 + 应用）
  root.querySelectorAll('[data-silence]').forEach((b) => b.addEventListener('click', async () => {
    const a = items[Number(b.dataset.silence)];
    if (!a) return;
    const mins = Number(window.prompt(`静默「${a.rule_name}」多少分钟？（1–10080）`, '60'));
    if (!mins) return;
    const reason = window.prompt('原因（可选）：', '') || '';
    const r = await api.alertSilenceCreate({ rule_id: a.rule_id, host: a.host, app: a.app, minutes: mins, reason });
    if (!r.ok) window.alert('静默失败：' + ((r.error && r.error.message) || ''));
    reload();
  }));

  // FR-804：解除静默 / 维护窗口
  root.querySelectorAll('[data-unsilence]').forEach((b) => b.addEventListener('click', async () => {
    const s = silences[Number(b.dataset.unsilence)];
    if (!s) return;
    if (!window.confirm('解除该静默？')) return;
    const r = await api.alertSilenceDelete(s.id);
    if (!r.ok) window.alert('解除失败：' + ((r.error && r.error.message) || ''));
    reload();
  }));
  const maintBtn = root.querySelector('#maint');
  if (maintBtn) maintBtn.addEventListener('click', async () => {
    if (maint) {
      if (!window.confirm('结束维护窗口并恢复告警？')) return;
      const r = await api.alertMaintenanceClear();
      if (!r.ok) window.alert('操作失败：' + ((r.error && r.error.message) || ''));
      reload();
      return;
    }
    const mins = Number(window.prompt('维护窗口时长（分钟，1–10080）：', '60'));
    if (!mins) return;
    const reason = window.prompt('原因（可选，例如“系统升级”）：', '') || '';
    const r = await api.alertMaintenanceSet({ minutes: mins, reason });
    if (!r.ok) window.alert('进入维护窗口失败：' + ((r.error && r.error.message) || ''));
    reload();
  });

  // FR-404：导出
  const expJson = root.querySelector('#alerts-json');
  const expCsv = root.querySelector('#alerts-csv');
  if (expJson) expJson.addEventListener('click', () => exportList('json', items));
  if (expCsv) expCsv.addEventListener('click', () => exportList('csv', items));

  const dl = root.querySelector('#selflog-dl');
  if (dl) {
    dl.addEventListener('click', async () => {
      try {
        const res = await fetch(api.appPrefix + 'api/v1/selflog/download?lines=2000', { credentials: 'include' });
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const blob = await res.blob();
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = 'log-server-for-tos-selflog.log';
        document.body.appendChild(a); a.click(); a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 4000);
      } catch (e) {
        dl.textContent = '下载失败：' + String((e && e.message) || e);
      }
    });
  }
}

function alertRow(a, i) {
  const lvl = a.level <= 2 ? 'crit' : 'warn';
  return `<div class="list-item${a.acked ? ' acked' : ''}">
    ${dot(a.acked ? 'off' : lvl)}
    <div style="min-width:0">
      <div class="name">${esc(a.rule_name)}（${esc(a.category)}）${a.acked ? ' ' + pill('已确认') : ''}</div>
      <div class="meta">${esc(a.level_text)} · ${esc(a.host || '-')} · ${esc(a.at)} · ${esc((a.sample_raw || '').slice(0, 90))}</div>
    </div>
    <div class="right">
      <span class="muted">×${esc(a.count)}</span>
      ${a.acked ? '' : `<span class="btn" data-ack="${i}" style="padding:1px 8px">确认</span>`}
      <span class="btn" data-silence="${i}" style="padding:1px 8px">静默</span>
      <span class="btn" data-ctx="${i}" style="padding:1px 8px">上下文</span>
    </div>
  </div>`;
}

function silenceRow(s, i) {
  const scope = s.maintenance
    ? '全局维护窗口'
    : ([s.rule_id && `规则 ${s.rule_id}`, s.host && `主机 ${s.host}`, s.app && `应用 ${s.app}`].filter(Boolean).join(' · ') || '全局');
  const until = new Date(s.until);
  return `<div class="list-item">
    ${dot(s.maintenance ? 'warn' : 'off')}
    <div style="min-width:0">
      <div class="name">${esc(scope)}</div>
      <div class="meta">到 ${esc(isNaN(until) ? s.until : until.toLocaleString())}${s.reason ? ' · ' + esc(s.reason) : ''}${s.by ? ' · ' + esc(s.by) : ''}</div>
    </div>
    <div class="right"><span class="btn" data-unsilence="${i}" style="padding:1px 8px">解除</span></div>
  </div>`;
}

/** FR-404：导出列表为 JSON / CSV（客户端对当前数据生成）。 */
function exportList(kind, items) {
  if (!items || !items.length) { window.alert('没有可导出的告警'); return; }
  let text, type, name;
  if (kind === 'csv') {
    const keys = ['rule_id', 'rule_name', 'category', 'level', 'level_text', 'host', 'at', 'count', 'acked', 'sample_raw'];
    const head = keys.join(',');
    const body = items.map(d => keys.map(k => `"${String(d[k] ?? '').replace(/"/g, '""')}"`).join(',')).join('\n');
    text = head + '\n' + body; type = 'text/csv'; name = 'alerts.csv';
  } else {
    text = JSON.stringify(items, null, 2); type = 'application/json'; name = 'alerts.json';
  }
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([text], { type }));
  a.download = 'log-server-for-tos-' + name;
  document.body.appendChild(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 5000);
}
