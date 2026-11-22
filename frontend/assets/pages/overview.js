/* pages/overview.js —— 总览（真实数据来自 GET /api/v1/overview）
 * 约束：总览全部数据来自后端聚合计数器，前端不做全量扫描（FR-1204）。
 */
import {
  card, kpis, progress, listItem, statusbar, notice, table,
  sparkline, demoSeries, demoPill, pill, pageHead, esc,
} from '../ui.js?v=__ASSET_VER__';
import { t } from '../i18n.js?v=__ASSET_VER__';
import * as api from '../api.js?v=__ASSET_VER__';

let timer = null;

export function onLeave() { if (timer) { clearInterval(timer); timer = null; } }
export function onEnter() { /* 页面自身负责轮询 */ }

export async function render(root) {
  const r = await api.overview(null);
  const offline = r.offline || !r.ok;
  const d = offline ? null : r.data;

  // 状态条只反映「本应用是否在运行/是否连得上后端」。
  // ★ 平台能力不在总览对用户展示（项目负责人要求移除）；如需查看见「关于 → 能力依赖说明」。
  const level = !d ? 'warn' : 'ok';
  const stateText = !d ? t('offline') : '运行中';

  const counters = d ? d.counters : { total: 0, by_class: {}, by_severity: {}, bytes: 0, buckets: 0 };
  const storage = d ? d.storage : { watermark_pct: 0, total_bytes: 0, retention_days: 14 };
  const rx = d ? (d.rx || {}) : {};
  const fwd = d ? (d.forward || []) : [];
  const alerts = d ? (d.alerts || {}) : {};
  const sources = d ? (d.sources || []) : [];
  const selfStats = d && d.self && d.self.log ? d.self.log : { used: 0, capacity: 2000, dropped: 0 };

  const series = d && d.series && d.series.length ? d.series.map(x => x.count) : demoSeries(30, 4, 20);
  const fmtBytes = (b) => {
    if (!b) return '0 B';
    const u = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0, v = Number(b);
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return v.toFixed(v < 10 && i > 0 ? 1 : 0) + ' ' + u[i];
  };
  const fmtUptime = (s) => {
    if (s == null) return '—';
    const n = Math.max(0, Math.floor(s));
    const d0 = Math.floor(n / 86400), h = Math.floor((n % 86400) / 3600), m = Math.floor((n % 3600) / 60);
    if (d0 > 0) return `${d0} 天 ${h} 小时`;
    if (h > 0) return `${h} 小时 ${m} 分`;
    if (m > 0) return `${m} 分 ${n % 60} 秒`;
    return `${n} 秒`;
  };

  root.innerHTML = [
    pageHead(t('nav.overview'), '',
      offline ? demoPill() : `<span class="pill ok">实时</span>`),

    statusbar(level, stateText, [
      ['运行时长', d ? fmtUptime(d.uptime_sec) : '—'],
      ['版本', d && d.version ? 'v' + d.version : '—'],
      ['资源档位', d && d.resource_mode ? d.resource_mode : '—'],
      ['24h 条数', String(counters.total)],
      ['存储引擎', d ? storage.engine : '—'],
    ]),

    '<div class="grid wide">',

    card({
      title: '采集健康',
      badge: pill(`${sources.length} 个源`),
      body: sources.length ? `
        <div class="list">${sources.map(s => listItem({
          level: (s.error ? 'crit' : (s.silent_for_sec > 600 ? 'warn' : 'ok')),
          name: s.path,
          meta: (s.error ? `错误：${s.error}` : `位点 ${s.offset} · 轮转 ${s.rotations} · 已采集 ${s.records}`) +
                (s.silent_for_sec >= 0 ? ` · ${s.silent_for_sec}s 前有新数据` : ''),
        })).join('')}</div>` :
        (offline ? `<div class="muted">后端未连接</div>`
          : `<div class="empty">尚未配置文件采集源（在 config.json 的 sources 中添加）</div>`),
    }),

    card({
      title: '接收健康',
      badge: pill(rx.udp_packets != null ? '运行中' : '—'),
      body: `${kpis([
        { v: rx.udp_packets || 0, k: 'UDP 报文' },
        { v: rx.tcp_conns || 0, k: 'TCP 连接' },
        { v: rx.dropped || 0, k: '丢弃' },
        { v: rx.queue_depth || 0, k: '入库队列' },
      ])}
      ${(rx.dropped > 0) ? notice('存在丢弃，请检查队列积压或来源速率', 'warn', 'warn') : ''}`,
    }),

    card({
      title: '存储健康',
      badge: pill(storage.watermark_pct > 85 ? '水位偏高' : '水位正常', storage.watermark_pct > 85 ? 'warn' : 'ok'),
      body: `
        ${kpis([
          { v: fmtBytes(storage.total_bytes), k: '日志占用' },
          { v: fmtBytes(storage.disk_free), k: '磁盘可用' },
          { v: String(storage.retention_days || 0), k: '保留天数', unit: '天' },
          { v: (storage.watermark_pct || 0).toFixed(1), k: '磁盘水位', unit: '%' },
        ])}
        ${progress(storage.watermark_pct || 0, storage.watermark_pct > 95 ? 'crit' : storage.watermark_pct > 85 ? 'warn' : '')}`,
    }),

    card({
      title: '转发健康',
      badge: pill(`${fwd.length} 个目标`),
      body: fwd.length ? `<div class="list">${fwd.map(f => listItem({
        level: f.healthy ? 'ok' : (f.enabled ? 'warn' : 'off'),
        name: `${f.name} (${f.protocol}://${f.addr})`,
        meta: `已发 ${f.sent} · 失败 ${f.failed} · 积压 ${f.queue.depth}` +
              (f.last_err ? ` · 最近错误：${f.last_err}` : ''),
        right: pill(f.enabled ? '启用' : '停用', f.enabled ? 'ok' : ''),
      })).join('')}</div>` : `<div class="empty">尚未配置转发目标</div>`,
    }),

    card({
      title: '告警与安全',
      badge: pill(`${(alerts.active || 0)} 条活跃`, (alerts.active || 0) > 0 ? 'crit' : 'ok'),
      cls: (alerts.active || 0) > 0 ? 'crit-border' : '',
      body: (alerts.items && alerts.items.length) ? `<div class="list">
        ${alerts.items.slice(0, 6).map(a => listItem({
          level: a.level <= 2 ? 'crit' : 'warn',
          name: a.rule_name,
          meta: `${a.level_text} · ${a.host || '-'} · ${a.sample_raw ? a.sample_raw.slice(0, 60) : ''}`,
          right: `<span class="muted">×${a.count}</span>`,
        })).join('')}</div>` : `<div class="empty">当前无告警</div>`,
    }),

    card({
      title: '日志量趋势（24h）',
      body: `${sparkline(series, { type: 'line' })}
        <div style="margin-top:8px">${kpis([
          { v: counters.total, k: '24h 总量' },
          { v: fmtBytes(counters.bytes), k: '原始字节' },
          { v: counters.buckets, k: '时间桶' },
          { v: counters.missed || 0, k: '窗口淘汰' },
        ])}</div>`,
    }),

    card({
      title: '按类别分布',
      body: Object.keys(counters.by_class || {}).length
        ? table(['类别', '条数'], Object.entries(counters.by_class).map(([k, v]) => [esc(k), esc(v)]))
        : `<div class="empty">暂无数据</div>`,
    }),

    card({
      title: '按级别分布',
      body: Object.keys(counters.by_severity || {}).length
        ? table(['级别', '条数'], Object.entries(counters.by_severity).map(([k, v]) => [esc(k), esc(v)]))
        : `<div class="empty">暂无数据</div>`,
    }),

    card({
      title: '应用自身日志',
      badge: pill(`${selfStats.used}/${selfStats.capacity}`),
      body: `${kpis([
        { v: selfStats.total || 0, k: '累计条数' },
        { v: selfStats.dropped || 0, k: '因上限丢弃' },
      ])}
      <div class="muted" style="font-size:11px;margin-top:6px">独立固定容量（防自噬 FR-1102）；水位告警时优先清理</div>
      <div style="margin-top:8px"><a class="btn" href="#/about">查看诊断入口</a></div>`,
    }),

    '</div>',
  ].join('');

  // 轻轮询（低资源档位：10s）
  if (timer) clearInterval(timer);
  timer = setInterval(() => { if (location.hash.startsWith('#/overview')) render(root); }, 10000);
}
