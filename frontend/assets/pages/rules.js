/* pages/rules.js —— 规则（真实规则来自 GET /api/v1/rules，支持试跑）
 * FR-803：通知通道配置（TOS 通知中心 / SMTP / Webhook）+ 发送测试 + 最近投递。
 */
import { card, table, notice, demoPill, pill, pageHead, esc, select } from '../ui.js?v=__ASSET_VER__';
import { t } from '../i18n.js?v=__ASSET_VER__';
import * as api from '../api.js?v=__ASSET_VER__';

const MIN_SEV = [['0', '全部级别'], ['3', 'err 及以上'], ['4', 'warning 及以上'], ['6', 'info 及以上']];

let CHANNELS = [];
let RECENT = [];

export async function render(root) {
  const [r, n] = await Promise.all([api.rules(null), api.notifyChannels(null)]);
  const offline = r.offline || !r.ok;
  const rules = (!offline && r.data.items) || [];
  CHANNELS = (!n.offline && n.ok && n.data && n.data.channels) || [];
  RECENT = (!n.offline && n.ok && n.data && n.data.recent) || [];
  const byKind = (k) => CHANNELS.find(c => c.kind === k) || { kind: k, enabled: false, min_severity: 0 };

  root.innerHTML = [
    pageHead(t('nav.rules'), '内置规则开箱可用 · 支持试跑预估命中 · 抑制聚合防刷屏',
      offline ? demoPill() : '<span class="pill ok">实时</span>'),

    notice('内置规则覆盖 DEV-REQUIREMENTS 附录 D 清单。同规则+同主机在抑制期内只累加计数（不重复告警）；频率规则在窗口内达标才触发（FR-804）。'),

    '<div class="grid wide" style="margin-top:12px">',

    card({
      title: '当前规则',
      badge: pill(`${rules.length} 条`),
      span: 'span-all',
      body: rules.length ? table(['类别', '规则', '级别', '频率/窗口', '状态'], rules.map(x => [
        esc(x.category), esc(x.name),
        pill(x.level <= 2 ? '严重' : '警告', x.level <= 2 ? 'crit' : 'warn'),
        x.threshold ? `${esc(x.threshold)} 次 / ${esc(Math.round((x.window || 0) / 1e9))}s` : '命中即告警',
        x.enabled ? pill('启用', 'ok') : pill('停用'),
      ])) : `<div class="empty">后端未连接</div>`,
    }),

    card({
      title: '规则试跑（FR-802）',
      span: 'span-all',
      body: `
        <div class="muted" style="font-size:12px;margin-bottom:8px">
          粘贴几行真实日志，保存前先看会命中哪些规则与条数，避免上线即刷屏。
        </div>
        <textarea id="dry-samples" class="field" style="width:100%;height:110px;font-family:ui-monospace,Menlo,monospace;font-size:12px"
          placeholder="kernel: Out of memory: Killed process 999&#10;sshd[1]: Failed password for root from 10.0.0.5"></textarea>
        <div style="margin-top:10px;display:flex;gap:8px">
          <button class="btn primary" id="dry-run">试跑</button>
        </div>
        <div id="dry-out" style="margin-top:10px"></div>`,
    }),

    card({
      title: '通知通道（FR-803）',
      span: 'span-all',
      badge: pill(`${CHANNELS.filter(c => c.enabled).length} 个已启用`),
      body: `
        <div class="muted" style="font-size:12px;margin-bottom:10px">
          告警触发时按通道分发；每个通道可设「最低级别」（只在达到该严重度及以上时推送）。
          <b>SMTP</b> 与 <b>Webhook</b> 由应用直接发送（不经过平台）。
          <b>SMTP 口令只保存在应用私有配置</b>，页面回显为 <span class="mono">***</span>。
        </div>
        <div class="notice warn" style="margin-bottom:10px">
          <b>TOS 通知中心（消息中心）暂未接入</b>：向 TOS 注入消息的官方通道/方法尚未提供（平台需求 R8）。
          在拿到官方方法前，应用<b>不向 TOS 写消息、也不注册通知类型</b>（D-017）。当前可用的推送通道为下面的 SMTP 与 Webhook。
        </div>
        ${chanBlock('smtp', byKind('smtp'))}
        ${chanBlock('webhook', byKind('webhook'))}
        <div style="display:flex;gap:8px;margin-top:12px">
          <button class="btn primary" id="nch-save">保存通知配置</button>
          <span class="muted" style="align-self:center;font-size:11px" id="nch-msg"></span>
        </div>
        <div style="margin-top:14px">
          <div class="muted" style="font-size:12px;margin-bottom:6px">最近投递（最多 50 条）</div>
          ${RECENT.length ? table(['时间', '通道', '目标', '结果'], RECENT.map(d => [
            esc(new Date(d.at).toLocaleString()),
            esc(d.kind),
            `<span class="mono">${esc((d.target || '').slice(0, 60))}</span>`,
            d.ok ? pill('成功', 'ok') : pill('失败：' + (d.error || ''), 'crit'),
          ])) : `<div class="empty">暂无投递记录</div>`}
        </div>`,
    }),

    '</div>',
  ].join('');

  bindRules(root);
  bindNotify(root);
}

/* ── 规则试跑（FR-802）────────────────────────────────────────── */
function bindRules(root) {
  const btn = root.querySelector('#dry-run');
  if (!btn) return;
  btn.addEventListener('click', async () => {
    const raw = root.querySelector('#dry-samples').value || '';
    const samples = raw.split('\n').map(s => s.trim()).filter(Boolean);
    const out = root.querySelector('#dry-out');
    if (!samples.length) { out.innerHTML = `<div class="notice warn">请先粘贴几行日志</div>`; return; }
    const res = await api.dryRunRules(samples);
    if (!res.ok) {
      out.innerHTML = `<div class="notice crit">试跑失败：${esc(res.error ? res.error.message : '')}</div>`;
      return;
    }
    const hits = res.data.hits || [];
    out.innerHTML = hits.length
      ? `<div class="notice">命中 ${hits.length} 条</div>` + table(['规则', '级别', '样例'],
          hits.slice(0, 20).map(h => [esc(h.name), esc(h.level), `<span class="mono">${esc((h.sample || '').slice(0, 70))}</span>`]))
      : `<div class="notice warn">没有命中任何规则（可以考虑补充自定义规则）</div>`;
  });
}

/* ── 通知通道（FR-803）────────────────────────────────────────── */
function chanBlock(kind, c) {
  const title = { tos: 'TOS 通知中心', smtp: 'SMTP 邮件', webhook: 'Webhook' }[kind] || kind;
  const body = {
    smtp: `
      <label>服务器</label><input id="smtp-host" class="field" placeholder="smtp.example.com" value="${esc(c.smtp_host || '')}">
      <label>端口</label><input id="smtp-port" class="field" type="number" placeholder="587" value="${esc(c.smtp_port || '')}">
      <label>用户名</label><input id="smtp-user" class="field" value="${esc(c.smtp_user || '')}">
      <label>口令</label><input id="smtp-pass" class="field" type="password" placeholder="（不回显）" value="${esc(c.smtp_pass || '')}">
      <label>发件人</label><input id="smtp-from" class="field" placeholder="logserver@example.com" value="${esc(c.smtp_from || '')}">
      <label>收件人</label><input id="smtp-to" class="field" placeholder="a@x.com, b@y.com" value="${esc((c.smtp_to || []).join(', '))}">
      <label>加密</label>
      ${select('smtp-tls', c.smtp_tls || 'starttls', [['starttls', 'STARTTLS（587）'], ['tls', '隐式 TLS（465）'], ['none', '明文（25）']])}`,
    webhook: `
      <label>URL</label><input id="wh-url" class="field" style="grid-column:span 3" placeholder="https://example.com/hook" value="${esc(c.url || '')}">
      <label>方法</label>
      ${select('wh-method', c.method || 'POST', [['POST', 'POST'], ['PUT', 'PUT']])}
      <label>自定义头</label><textarea id="wh-headers" class="field" style="grid-column:span 3;height:56px;font-family:ui-monospace,Menlo,monospace;font-size:12px"
        placeholder='{"Authorization":"Bearer ..."}'>${esc(JSON.stringify(c.headers || {}, null, 1))}</textarea>`,
  }[kind];

  return `<div class="card" style="background:var(--surface2);margin-bottom:10px">
    <div class="card-head">
      <h3>${title}</h3>
      <label style="margin-left:auto;display:flex;gap:6px;align-items:center;font-size:13px">
        <input type="checkbox" id="${kind}-en" ${c.enabled ? 'checked' : ''}> 启用
      </label>
    </div>
    <div style="display:grid;grid-template-columns:auto 1fr;gap:8px 10px;align-items:center;font-size:13px">
      ${body}
      <label>最低级别</label>
      ${select(kind + '-min', String(c.min_severity || 0), MIN_SEV)}
    </div>
    <div style="margin-top:10px">
      <span class="btn" data-test="${kind}">发送测试</span>
      <span class="muted" style="font-size:11px;margin-left:8px" id="${kind}-test-msg"></span>
    </div>
  </div>`;
}

function bindNotify(root) {
  root.querySelectorAll('[data-test]').forEach(b => b.addEventListener('click', async () => {
    const kind = b.dataset.test;
    const msg = root.querySelector('#' + kind + '-test-msg');
    b.textContent = '发送中…';
    const r = await api.testNotify(kind);
    b.textContent = '发送测试';
    if (msg) msg.textContent = r.ok ? '✅ 已发送（请到对应终端确认）' : ('❌ ' + ((r.error && r.error.message) || '失败'));
  }));

  const save = root.querySelector('#nch-save');
  if (!save) return;
  save.addEventListener('click', async () => {
    const msg = root.querySelector('#nch-msg');
    const channels = collectChannels(root);
    const r = await api.putNotifyChannels(channels);
    if (msg) msg.textContent = r.ok ? '✅ 已保存并生效' : ('❌ ' + ((r.error && r.error.message) || '保存失败'));
    if (r.ok) { const m = await api.notifyChannels(null); if (m.ok) { RECENT = m.data.recent || []; } }
  });
}

function collectChannels(root) {
  const $ = (id) => root.querySelector('#' + id);
  const val = (id) => ($(id) ? $(id).value : '');
  const chk = (id) => !!($(id) && $(id).checked);
  let headers = {};
  try { headers = JSON.parse(val('wh-headers') || '{}'); } catch { headers = {}; }
  return [
    {
      kind: 'smtp', enabled: chk('smtp-en'), min_severity: Number(val('smtp-min') || 0),
      smtp_host: val('smtp-host').trim(), smtp_port: Number(val('smtp-port') || 0),
      smtp_user: val('smtp-user').trim(), smtp_pass: val('smtp-pass'),
      smtp_from: val('smtp-from').trim(),
      smtp_to: val('smtp-to').split(',').map(s => s.trim()).filter(Boolean),
      smtp_tls: val('smtp-tls') || 'starttls',
    },
    {
      kind: 'webhook', enabled: chk('wh-en'), min_severity: Number(val('wh-min') || 0),
      url: val('wh-url').trim(), method: val('wh-method') || 'POST', headers,
    },
  ];
}
