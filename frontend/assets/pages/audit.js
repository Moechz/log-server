/* pages/audit.js —— 审计板块（与「监控」分板块、共数据；能力状态如实展示）
 * FR-404：审计事件可导出（JSON / CSV）；FR-406/807：每条事件可看上下文。
 */
import { card, kv, listItem, table, notice, demoPill, pill, pageHead, esc, empty } from '../ui.js?v=__ASSET_VER__';
import { t } from '../i18n.js?v=__ASSET_VER__';
import * as api from '../api.js?v=__ASSET_VER__';

const EVENTS_LIMIT = 200;

export async function render(root, ctx) {
  const from24 = new Date(Date.now() - 24 * 3600 * 1000).toISOString();
  const [or, al, lg, ar] = await Promise.all([
    api.overview(null),
    api.alerts(50, null),
    api.logs({ class: 'auth', from: from24, limit: EVENTS_LIMIT }, null),
    api.auditActions(100),
  ]);
  const offline = or.offline || !or.ok;
  const d = offline ? null : or.data;
  const audit = d ? (d.audit || {}) : {};
  const counters = d ? d.counters : { by_class: {} };
  const alerts = (!al.offline && al.ok && al.data.items) || [];
  const events = (!lg.offline && lg.ok && lg.data && Array.isArray(lg.data.Logs)) ? lg.data.Logs : [];
  const actions = (!ar.offline && ar.ok && ar.data && Array.isArray(ar.data.items)) ? ar.data.items : [];
  const astats = (!ar.offline && ar.ok && ar.data && ar.data.stats) ? ar.data.stats : {};
  void audit;

  const r2 = d ? d.capabilities.find(c => c.id === 'R2') : null;
  const r3 = d ? d.capabilities.find(c => c.id === 'R3') : null;
  const auditAlerts = alerts.filter(a => ['ssh-brute', 'ssh-accept', 'sudo-denied', 'acct-change', 'audit-lost'].includes(a.rule_id));

  root.innerHTML = [
    pageHead(t('nav.audit'), '谁在何时对什么做了什么，结果如何 —— 与监控共用同一份数据',
      offline ? demoPill() : '<span class="pill ok">实时</span>'),

    notice('本阶段为「审计就绪（Audit-Ready）」：**不宣称合规**。**R2** 影响宿主侧是否保留历史（决定停机缺口能否回填）；**R3** 决定能否提供 syscall 级证据。二者都不做绕过（D-017）。', 'warn', 'warn'),

    '<div class="grid wide" style="margin-top:12px">',

    card({
      title: '审计能力状态',
      badge: (r2 && r2.state === 'available' && r3 && r3.state === 'available') ? pill('就绪', 'ok') : pill('待平台支持', 'warn'),
      body: kv([
        ['R2 系统日志持久化（仅影响缺口/回填）', r2 ? (r2.state === 'available' ? '可用' : r2.reason || '不可用') : '—'],
        ['R3 主机审计能力', r3 ? (r3.state === 'available' ? '可用' : r3.reason || '不可用') : '—'],
        ['审计数据条数（近 24h）', String((counters.by_class && counters.by_class['auth']) || 0)],
        ['完整性校验', '未运行（阶段三）'],
        ['留存锁定', '未启用（阶段三）'],
      ]),
    }),

    card({
      title: '审计类事件（近 24h）',
      span: 'span-all',
      badge: pill(`${events.length} 条`),
      body: `
        <div style="display:flex;gap:8px;margin-bottom:10px;flex-wrap:wrap">
          <span class="btn" id="audit-json">导出 JSON</span>
          <span class="btn" id="audit-csv">导出 CSV</span>
          <span class="muted" style="font-size:11px;align-self:center">
            审计类记录与其他日志共用**全局保留期**（见「存储」页）；180 天分档尚未实现（阶段二）。导出的是**当前展示结果**（FR-404）。
          </span>
        </div>
        ${events.length ? table(
          ['时间 (UTC)', '级别', '主机', '应用', '消息', '操作'],
          events.slice(0, 100).map((e, i) => [
            esc(String(e['_time'] || '')),
            esc(String(e['ls_level'] || e['ls_severity_number'] || '—')),
            esc(String(e['ls_host'] || '—')),
            esc(String(e['ls_app'] || '—')),
            esc(String(e['ls_msg'] || e['_msg'] || '').slice(0, 140)),
            `<span class="btn" data-ctx="${i}" style="padding:1px 8px">上下文</span>`,
          ]),
        ) : empty(offline ? '后端未连接' : '近 24h 无审计类事件（sshd/账户变更等落库后此处会显示）')}`,
    }),

    card({
      title: '审计相关告警',
      span: 'span-all',
      body: auditAlerts.length
        ? `<div class="list">${auditAlerts.map((a, i) => listItem({
            level: a.level <= 2 ? 'crit' : 'warn',
            name: a.rule_name,
            meta: `${a.level_text} · ${a.host || '-'} · ${a.at} · ${a.sample_raw ? a.sample_raw.slice(0, 80) : ''}`,
            right: `<span class="btn" data-actx="${i}" style="padding:1px 8px">上下文</span>`,
          })).join('')}</div>`
        : `<div class="empty">无审计相关告警</div>`,
    }),

    card({
      title: '管理操作留痕',
      span: 'span-all',
      badge: ar.ok ? pill(`${actions.length} 条`, 'accent') : pill('仅管理员可读', 'warn'),
      body: (ar.ok
        ? table(
            ['时间 (UTC)', '操作者', '动作', '对象', '结果', '详情', '来源 IP'],
            actions.map(a => [
              esc(String(a.ts || '')),
              esc(`${a.actor || '-'}${a.role ? ' / ' + a.role : ''}`),
              esc(String(a.action || '')),
              esc(String(a.target || '-')),
              esc(String(a.result || '')),
              esc(String(a.detail || '').slice(0, 120)),
              esc(String(a.ip || '-')),
            ]),
          )
        : notice('管理操作留痕需要管理员权限（设置页解锁后刷新）。', 'info', 'info')) +
        `<div class="muted" style="font-size:11px;margin-top:8px">
          独立文件 <span class="mono">${esc(String(astats.path || '<数据目录>/audit/actions.log'))}</span>：
          追加写、写入前脱敏、单文件 10 MB 轮转、总量 ${esc(String(astats.max_total_mb || 200))} MB、
          保留 ${esc(String(astats.max_age_days || 180))} 天；<b>不随日志保留期删除</b>。
          当前 ${esc(String(astats.files || 0))} 个文件 / ${esc(String(astats.bytes || 0))} 字节。
          注意：这是应用层追加日志，不是 WORM，能读数据目录的人仍可删除/篡改。
        </div>`,
    }),

    card({
      title: '审计访问留痕（设计）',
      span: 'span-all',
      body: `<div class="muted" style="font-size:12px">
        查看、导出、删除审计数据的行为本身必须被记录（FR-707）。当前实现：**管理操作**（改配置、导出/导入、
        存储位置、暂停采集、告警确认/静默、管理员认领）已写入独立的管理操作留痕（见上一张卡）；
        **对审计数据的查看/导出**会随阶段三审计模式一起补齐（FR-707）。
      </div>`,
    }),

    '</div>',
  ].join('');

  // FR-807/406：审计事件 / 审计告警 → 上下文
  const goCtx = (p) => { if (ctx && typeof ctx.navigate === 'function') ctx.navigate('search', p); };
  root.querySelectorAll('[data-ctx]').forEach(b => b.addEventListener('click', () => {
    const e = events.slice(0, 100)[Number(b.dataset.ctx)];
    if (!e || !e['_time']) return;
    goCtx({ pivot: e['_time'], class: 'auth', ...(e['ls_host'] ? { host: e['ls_host'] } : {}) });
  }));
  root.querySelectorAll('[data-actx]').forEach(b => b.addEventListener('click', () => {
    const a = auditAlerts[Number(b.dataset.actx)];
    if (!a) return;
    goCtx({ pivot: a.at, class: 'auth', ...(a.host ? { host: a.host } : {}), ...(a.sample_raw ? { q: a.sample_raw.slice(0, 120) } : {}) });
  }));

  // FR-404：导出
  const j = root.querySelector('#audit-json');
  const c = root.querySelector('#audit-csv');
  if (j) j.addEventListener('click', () => exportRows('json', events));
  if (c) c.addEventListener('click', () => exportRows('csv', events));
}

function exportRows(kind, rows) {
  if (!rows || !rows.length) { window.alert('没有可导出的审计事件'); return; }
  let text, type, name;
  if (kind === 'csv') {
    const keys = ['_time', 'ls_level', 'ls_severity_number', 'ls_host', 'ls_app', '_msg'];
    const head = keys.join(',');
    const body = rows.map(d => keys.map(k => `"${String(d[k] ?? '').replace(/"/g, '""')}"`).join(',')).join('\n');
    text = head + '\n' + body; type = 'text/csv'; name = 'audit-events.csv';
  } else {
    text = JSON.stringify(rows, null, 2); type = 'application/json'; name = 'audit-events.json';
  }
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([text], { type }));
  a.download = 'log-server-for-tos-' + name;
  document.body.appendChild(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 5000);
}
