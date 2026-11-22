/* pages/search.js —— 检索（**真实接口**：GET /api/v1/logs）
 *
 * 能力：关键字/正则全文 + 主机 + 应用 + 板块(class) + 最低级别(min_severity) + 时间范围 + limit。
 * FR-403：时间轴直方图，**拖动框选 → 下钻**（单击柱子放大到该时间段）。
 * FR-406：任意一条结果可看**上下文**（前 N / 后 N 条）；也被「监控 → 告警」的 FR-807 跳转复用。
 * FR-404：把当前结果导出为 JSON / CSV（客户端生成）。
 * 翻页：上一页/下一页（时间倒序游标）；暂未实现：服务端异步导出（FR-404 的异步部分）。
 */
import { card, table, pill, kv, notice, empty, pageHead, demoPill, esc, select } from '../ui.js?v=__ASSET_VER__';
import { t } from '../i18n.js?v=__ASSET_VER__';
import * as api from '../api.js?v=__ASSET_VER__';

const RANGE_MIN = { '15m': 15, '1h': 60, '24h': 1440, '7d': 10080 };
const RANGES = [['15m', '最近 15 分钟'], ['1h', '最近 1 小时'], ['24h', '最近 24 小时'], ['7d', '最近 7 天'], ['custom', '自定义…']];
const CLASSES = [['', '全部板块'], ['auth', '审计 / 认证'], ['system', '系统 / 内核'], ['web', 'Web / HTTP']];
const LEVELS = [['', '全部级别'], ['3', 'err 及以上'], ['4', 'warning 及以上'], ['6', 'info 及以上']];
const LIMITS = [50, 100, 200];
const MAX_ROWS = 200;      // 客户端渲染上限
const CTX_N = 20;          // 上下文前后条数

const state = {
  root: null, q: '', range: '1h', from: '', to: '', cls: '', app: '', host: '',
  minSev: '', limit: 100, rows: [], pivot: '', hist: null,
  page: 0, bounds: [null], nextCursor: '',
};

export async function render(root, ctx) {
  state.root = root;
  applyQuery(ctx && ctx.query);
  const customOpen = state.range === 'custom';

  root.innerHTML = [
    pageHead(t('nav.search'), '全文与字段查询 · 原始 / 解析双视图', '<span class="pill">实时接口</span>'),

    notice('检索走存储适配层（FR-312），条件按 <span class="mono">ls_*</span> 字段下发：' +
      '关键字用引擎全文/正则；类别、级别、主机、应用为结构化过滤。原文：UTF-8 日志在 <span class="mono">_msg</span> 逐字节保留；' +
      '非 UTF-8 日志的原始字节另存 <span class="mono">ls_raw_b64</span>（base64，可还原）。' +
      '<b>本阶段不宣称合规</b>（D-010）。'),

    '<div class="grid wide" style="margin-top:12px">',

    card({
      title: '查询条件',
      span: 'span-all',
      body: `
        <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:center">
          <input id="sq" class="field" style="flex:1;min-width:240px"
                 placeholder="关键字 / 正则，例如：Failed password&#10;（留空 = 全部）">
          ${select('sr', state.range, RANGES)}
          ${select('sc', state.cls, CLASSES)}
          ${select('sl', state.minSev, LEVELS)}
          ${select('sn', String(state.limit), LIMITS.map(n => [String(n), n + ' 条']))}
          <input id="shost" class="field" style="width:150px" placeholder="主机 host=" value="${esc(state.host)}">
          <input id="sapp" class="field" style="width:150px" placeholder="应用 app=" value="${esc(state.app)}">
          <button class="btn primary" id="sgo">检索</button>
          <button class="btn" id="sexp-json">导出 JSON</button>
          <button class="btn" id="sexp-csv">导出 CSV</button>
        </div>
        <div id="scustom" style="display:${customOpen ? 'flex' : 'none'};margin-top:8px;gap:8px;align-items:center">
          <input id="sfrom" class="field mono" type="datetime-local" style="width:210px" value="${esc(toLocalInput(state.from))}">
          <span class="muted">→</span>
          <input id="sto" class="field mono" type="datetime-local" style="width:210px" value="${esc(toLocalInput(state.to))}">
          <span class="muted" style="font-size:11px">（时间轴拖动选择会自动填入这里）</span>
        </div>
        <div style="margin-top:10px;display:flex;gap:6px;flex-wrap:wrap">
          ${['q= 全文', 'host=', 'app=', 'class=', 'min_severity<=', 'from=/to= (RFC3339)'].map(f => pill(f)).join('')}
        </div>`,
    }),

    card({
      title: '时间轴（拖动框选下钻）',
      span: 'span-all',
      badge: '<span id="tl-badge" class="pill">—</span>',
      body: '<div id="s-timeline"><div class="muted" style="font-size:12px">加载中…</div></div>',
    }),

    card({
      title: '结果',
      badge: '<span id="smeta" class="pill">未检索</span>',
      span: 'span-all',
      body: '<div id="sctx"></div><div id="sout">' + empty('点「检索」开始') + '</div>',
    }),

    card({
      title: '说明与边界',
      body: kv([
        ['翻页', '已支持「上一页 / 下一页」——按时间倒序游标翻页（新日志写入不会造成重复/遗漏）；每页条数可选 50/100/200'],
        ['服务端异步导出（FR-404）', '当前导出为客户端对**当前结果**生成 JSON / CSV（异步导出待后续）'],
        ['按用户分权', '依赖平台「查询当前用户/角色」端点（官方未提供）；当前所有已登录用户按管理员处理'],
        ['上下文（FR-406）', `前 / 后各 ${CTX_N} 条，按 ` + '`_time`' + ' 排序取最靠近的记录（与 VictoriaLogs 官方 UI 同口径）'],
      ]),
    }),

    '</div>',
  ].join('');

  const $ = (id) => root.querySelector('#' + id);
  $('sq').value = state.q;
  $('sr').addEventListener('change', () => {
    state.range = $('sr').value;
    $('scustom').style.display = state.range === 'custom' ? 'flex' : 'none';
    loadHistogram();
  });
  $('sc').addEventListener('change', () => { state.cls = $('sc').value; });
  $('sl').addEventListener('change', () => { state.minSev = $('sl').value; });
  $('sn').addEventListener('change', () => { state.limit = Number($('sn').value) || 100; run(true); });
  $('shost').addEventListener('change', () => { state.host = $('shost').value; });
  $('sapp').addEventListener('change', () => { state.app = $('sapp').value; });
  $('sgo').addEventListener('click', () => { run(true); loadHistogram(); });
  $('sq').addEventListener('keydown', (e) => { if (e.key === 'Enter') { run(true); loadHistogram(); } });
  $('sexp-json').addEventListener('click', () => exportRows('json'));
  $('sexp-csv').addEventListener('click', () => exportRows('csv'));
  ['sfrom', 'sto'].forEach(id => $(id).addEventListener('change', () => {
    state.from = $('sfrom').value; state.to = $('sto').value; run(); loadHistogram();
  }));

  await run();
  await loadHistogram();
  if (state.pivot) openContext(state.pivot);
}

/* ── 从 URL 参数恢复（FR-807：告警跳转、分享链接）──────────────── */
function applyQuery(q) {
  if (!q) return;
  if (q.q) state.q = q.q;
  if (q.host) state.host = q.host;
  if (q.app) state.app = q.app;
  if (q.class) state.cls = q.class;
  if (q.min_severity) state.minSev = q.min_severity;
  if (q.limit) state.limit = Number(q.limit) || state.limit;
  if (q.range && (RANGE_MIN[q.range] || q.range === 'custom')) state.range = q.range;
  if (q.from) { state.range = 'custom'; state.from = q.from; }
  if (q.to) state.to = q.to;
  if (q.pivot) state.pivot = q.pivot;
}

/* ── 查询参数 ─────────────────────────────────────────────────── */
function filters() {
  const p = {};
  if (state.q.trim()) p.q = state.q.trim();
  if (state.cls) p.class = state.cls;
  if (state.host.trim()) p.host = state.host.trim();
  if (state.app.trim()) p.app = state.app.trim();
  if (state.minSev) p.min_severity = state.minSev;
  if (state.range === 'custom') {
    if (state.from) p.from = new Date(state.from).toISOString();
    if (state.to) p.to = new Date(state.to).toISOString();
  } else {
    const mins = RANGE_MIN[state.range] || 60;
    p.from = new Date(Date.now() - mins * 60000).toISOString();
  }
  return p;
}
const params = () => ({ ...filters(), limit: state.limit });

async function run(reset = true) {
  const root = state.root;
  if (!root) return;
  const $ = (id) => root.querySelector('#' + id);
  state.q = $('sq').value;
  state.host = $('shost').value;
  state.app = $('sapp').value;
  if (state.range === 'custom') { state.from = $('sfrom').value; state.to = $('sto').value; }

  // 重新检索 → 回到第 1 页；翻页（reset=false）沿用游标栈
  if (reset) { state.page = 0; state.bounds = [null]; state.nextCursor = ''; state.pivot = ''; }

  $('smeta').textContent = '检索中…';
  $('sout').innerHTML = empty('检索中…');

  const bound = state.bounds[state.page] || '';
  const r = await api.logs({ ...params(), ...(bound ? { to: bound } : {}) }, null);
  if (r.offline || !r.ok || !r.data) {
    state.rows = [];
    $('smeta').outerHTML = '<span id="smeta" class="pill warn">后端未连接</span>';
    $('sout').innerHTML = notice('未连接到后端（显示的为示例数据）。部署后此页会直接查询 VictoriaLogs。', 'warn') +
      demoRows();
    return;
  }
  let rows = Array.isArray(r.data.Logs) ? r.data.Logs : [];
  // 游标边界去重：以边界时间戳为 to 时，若引擎把边界那条也带回，则丢掉它（避免上一页最后一条重复出现）
  if (bound) rows = rows.filter(x => String(x['_time'] || '') < bound);
  state.rows = rows.slice(0, MAX_ROWS);
  state.nextCursor = r.data.NextCursor || '';
  if (state.nextCursor) state.bounds[state.page + 1] = state.nextCursor;

  const took = r.data.TookMS != null ? `${r.data.TookMS} ms` : '—';
  $('smeta').outerHTML = `<span id="smeta" class="pill ok">第 ${state.page + 1} 页 · ${state.rows.length} 条 · ${esc(took)}</span>`;

  if (!state.rows.length) {
    $('sout').innerHTML = empty(state.page > 0 ? '本页没有更多日志（已是最后一页）' : '没有匹配的日志（可放宽时间范围或去掉关键字）') + pagerHTML();
    bindPager(root);
    return;
  }
  $('sout').innerHTML = table(
    ['时间 (UTC)', '级别', '主机', '应用', '来源', '消息', '操作'],
    state.rows.map((d, i) => [
      esc(String(d['_time'] || '')),
      esc(levelText(d)),
      esc(String(d['ls_host'] || '—')),
      esc(String(d['ls_app'] || '—')),
      esc(String(d['ls_source_kind'] || '—')),
      esc(String(d['ls_msg'] || d['_msg'] || '').slice(0, 160)),
      `<span class="btn" data-ctx="${i}" style="padding:1px 8px">上下文</span>
       <span class="btn" data-raw="${i}" style="padding:1px 8px">原文</span>`,
    ]),
  ) + pagerHTML() + '<div id="sraw" style="margin-top:10px"></div>';

  root.querySelectorAll('[data-raw]').forEach((b) => {
    b.addEventListener('click', () => showRaw(Number(b.dataset.raw)));
  });
  root.querySelectorAll('[data-ctx]').forEach((b) => {
    b.addEventListener('click', () => {
      const d = state.rows[Number(b.dataset.ctx)];
      if (d && d['_time']) openContext(String(d['_time']));
    });
  });
  bindPager(root);
}

/** 翻页控件（时间倒序、按游标翻页——新日志写入不会造成重复/遗漏）。 */
function pagerHTML() {
  return `<div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap;margin-top:10px">
    <span class="btn" id="pg-prev" ${state.page <= 0 ? 'disabled' : ''}>上一页</span>
    <span class="muted">第 <b>${state.page + 1}</b> 页 · 每页 ${state.limit} 条</span>
    <span class="btn" id="pg-next" ${state.nextCursor ? '' : 'disabled'}>下一页</span>
    <span class="muted" style="font-size:11px">按时间倒序翻页；「下一页」更早、「上一页」更新</span>
  </div>`;
}

function bindPager(root) {
  const prev = root.querySelector('#pg-prev');
  if (prev && !prev.hasAttribute('disabled')) {
    prev.addEventListener('click', () => { state.page--; run(false); });
  }
  const next = root.querySelector('#pg-next');
  if (next && !next.hasAttribute('disabled')) {
    next.addEventListener('click', () => { state.page++; run(false); });
  }
}

/* ── 时间轴（FR-403）───────────────────────────────────────────── */
async function loadHistogram() {
  const box = state.root && state.root.querySelector('#s-timeline');
  const badge = state.root && state.root.querySelector('#tl-badge');
  if (!box) return;
  box.innerHTML = '<div class="muted" style="font-size:12px">加载时间轴…</div>';
  const r = await api.logsHistogram(filters());
  if (!r.ok || !r.data) {
    box.innerHTML = `<div class="muted" style="font-size:12px">时间轴暂不可用（后端未连接）</div>`;
    if (badge) badge.textContent = '—';
    return;
  }
  state.hist = { buckets: densify(r.data.buckets || [], r.data.from, r.data.to, r.data.interval_sec || 0), intervalSec: r.data.interval_sec || 0 };
  if (badge) badge.textContent = `${state.hist.buckets.length} 桶 · 合计 ${r.data.total || 0} 条`;
  renderTimeline();
}

/** VL 只返回非空桶且按间隔对齐（epoch 对齐）→ 补齐空缺，保证时间轴位置准确。 */
function densify(buckets, fromIso, toIso, intervalSec) {
  if (!intervalSec || !fromIso || !toIso) return buckets;
  const step = intervalSec * 1000;
  const map = new Map(buckets.map(b => [Math.floor(new Date(b.bucket).getTime() / step) * step, Number(b.count) || 0]));
  const from = new Date(fromIso).getTime();
  const to = new Date(toIso).getTime();
  if (!(step > 0) || !(to > from)) return buckets;
  const t0 = Math.floor(from / step) * step;
  const out = [];
  for (let t = t0; t < to; t += step) {
    out.push({ bucket: new Date(t).toISOString(), count: map.get(t) || 0 });
  }
  return out.length ? out : buckets;
}

function renderTimeline() {
  const box = state.root && state.root.querySelector('#s-timeline');
  if (!box) return;
  const h = state.hist;
  if (!h || !h.buckets.length) {
    box.innerHTML = '<div class="empty">该时间范围内没有日志</div>';
    return;
  }
  const max = Math.max(1, ...h.buckets.map(b => Number(b.count) || 0));
  box.innerHTML = `
    <div class="tl" id="tl">
      ${h.buckets.map((b, i) => `<div class="tl-col" data-i="${i}"
        title="${esc(localTime(b.bucket))} · ${esc(b.count)} 条"><i style="height:${Math.max(2, Math.round((Number(b.count) || 0) / max * 100))}%"></i></div>`).join('')}
      <div class="tl-sel" id="tl-sel" hidden></div>
    </div>
    <div class="muted" style="font-size:11px;margin-top:6px">
      ${esc(localTime(h.buckets[0].bucket))} ～ ${esc(localTime(lastBucketEnd()))} · 每根柱子 = ${esc(h.intervalSec)} 秒；
      <b>拖动</b>选择范围下钻，<b>单击</b>柱子放大到该段。
    </div>`;
  bindTimeline();
}

function bindTimeline() {
  const tl = state.root.querySelector('#tl');
  if (!tl) return;
  const cols = Array.from(tl.querySelectorAll('.tl-col'));
  const sel = tl.querySelector('#tl-sel');
  let start = -1, cur = -1, dragging = false;

  const idxOf = (e) => {
    const el = e.target.closest ? e.target.closest('.tl-col') : null;
    return el ? Number(el.dataset.i) : -1;
  };
  const paint = () => {
    if (start < 0 || cur < 0) { sel.hidden = true; return; }
    const a = Math.min(start, cur), b = Math.max(start, cur);
    sel.hidden = false;
    sel.style.left = (a / cols.length * 100) + '%';
    sel.style.width = ((b - a + 1) / cols.length * 100) + '%';
  };
  const commit = () => {
    if (start < 0 || cur < 0) return;
    const a = Math.min(start, cur), b = Math.max(start, cur);
    const bs = state.hist.buckets;
    const from = new Date(bs[a].bucket);
    const to = new Date(new Date(bs[b].bucket).getTime() + state.hist.intervalSec * 1000);
    state.range = 'custom';
    state.from = toLocalInput(from.toISOString());
    state.to = toLocalInput(to.toISOString());
    state.pivot = '';
    const root = state.root;
    if (root.querySelector('#sfrom')) root.querySelector('#sfrom').value = state.from;
    if (root.querySelector('#sto')) root.querySelector('#sto').value = state.to;
    const sc = root.querySelector('#scustom'); if (sc) sc.style.display = 'flex';
    const sr = root.querySelector('#sr'); if (sr && sr.setValue) sr.setValue('custom');
    run(true); loadHistogram();
  };

  tl.addEventListener('mousedown', (e) => {
    const i = idxOf(e); if (i < 0) return;
    dragging = true; start = i; cur = i; paint(); e.preventDefault();
  });
  tl.addEventListener('mouseover', (e) => {
    if (!dragging) return;
    const i = idxOf(e); if (i < 0 || i === cur) return;
    cur = i; paint();
  });
  document.addEventListener('mouseup', () => {
    if (!dragging) return;
    dragging = false;
    if (start === cur) {
      // 单击：放大到该桶
      start = cur = Math.max(0, start);
    }
    commit();
  }, { once: false });
}

function lastBucketEnd() {
  const bs = state.hist.buckets;
  return new Date(new Date(bs[bs.length - 1].bucket).getTime() + state.hist.intervalSec * 1000).toISOString();
}

/* ── 上下文（FR-406 / FR-807）─────────────────────────────────── */
async function openContext(pivotIso) {
  const root = state.root;
  const box = root && root.querySelector('#sctx');
  if (!box) return;
  box.innerHTML = `<div class="muted" style="font-size:12px;margin-bottom:8px">加载 ${esc(pivotIso)} 的上下文…</div>`;
  const r = await api.logsContext({ ...filters(), t: pivotIso, before: CTX_N, after: CTX_N });
  if (!r.ok || !r.data) {
    box.innerHTML = notice('上下文加载失败：' + ((r.error && r.error.message) || '后端未连接'), 'crit', 'warn');
    return;
  }
  const before = r.data.before || [];
  const after = r.data.after || [];
  const line = (e, cls = '') => `<span class="ln"><span class="ts">${esc(String(e['_time'] || '').slice(11, 19))}</span>
    <span class="${cls}">${esc(String(e['ls_host'] || '-'))} ${esc(String(e['ls_level'] || ''))} ${esc(String(e['ls_msg'] || e['_msg'] || '').slice(0, 200))}</span></span>`;

  box.innerHTML = card({
    title: '日志上下文（前 ' + before.length + ' / 后 ' + after.length + ' 条）',
    badge: '<span class="btn" id="sctx-close" style="padding:1px 8px">关闭</span>',
    body: `<div class="log-panel" style="max-height:420px">
        ${before.map(e => line(e)).join('')}
        <span class="ln hl"><span class="ts">▶</span> 定位点 ${esc(pivotIso)}（筛选条件下的目标时间）</span>
        ${after.map(e => line(e)).join('')}
      </div>
      <div class="muted" style="font-size:11px;margin-top:6px">
        上下文同样应用当前筛选（q / host / app / class / min_severity）；按时间取最靠近定位点的前后各 ${CTX_N} 条。
      </div>`,
  });
  const close = box.querySelector('#sctx-close');
  if (close) close.addEventListener('click', () => { box.innerHTML = ''; state.pivot = ''; });
  box.scrollIntoView({ block: 'nearest' });
}

/* ── 原文 / 导出 ──────────────────────────────────────────────── */
function levelText(d) {
  const sev = d['ls_severity_number'];
  const lv = d['ls_level'];
  if (lv) return sev != null ? `${lv}(${sev})` : String(lv);
  return sev != null ? `sev ${sev}` : '—';
}

function showRaw(i) {
  const d = state.rows[i];
  if (!d || !state.root) return;
  const meta = [
    ['_time（落库时间，D-019）', d['_time']],
    ['ls_ingest_time（接收时间）', d['ls_ingest_time']],
    ['ls_event_time_raw（原文时间）', d['ls_event_time_raw']],
    ['ls_time_trusted / inferred', `${d['ls_time_trusted']} / ${d['ls_time_inferred'] ?? false}`],
    ['来源', `${d['ls_source_kind'] || ''} ${d['ls_protocol'] || ''} ${d['ls_source_ip'] || ''}`],
    ['编码 / 格式', `${d['ls_encoding'] || ''} / ${d['ls_format'] || ''}`],
    ['分类 / 保留档案', `${d['ls_class_primary'] || ''} [${d['ls_classes'] || ''}] · ${d['ls_retention_profile'] || ''}`],
  ];
  const box = state.root && state.root.querySelector('#sraw');
  if (!box) return;
  box.innerHTML = card({
    title: `第 ${i + 1} 条：原文与信封 / 派生字段`,
    body: kv(meta) + `<div class="log-panel" style="margin-top:8px">
      <span class="ln"><span class="ts">${d['ls_raw_b64'] ? 'utf8' : 'raw'}</span> ${esc(String(d['_msg'] || ''))}</span>
      ${d['ls_raw_b64'] ? `<span class="ln"><span class="ts">b64</span> ${esc(String(d['ls_raw_b64']))}</span>
      <span class="ln"><span class="ts">orig</span> ${esc(String(d['ls_raw_encoding'] || 'unknown'))} · ${esc(String(d['ls_raw_bytes'] ?? ''))} bytes（base64 可还原原始字节）</span>` : ''}
      <span class="ln"><span class="ts">drv</span> ls_level=${esc(d['ls_level'] ?? '')} ls_severity_number=${esc(d['ls_severity_number'] ?? '')} ls_parse_error=${esc(d['ls_parse_error'] ?? '')}</span>
    </div>`,
  });
}

function exportRows(kind) {
  if (!state.rows.length) { return; }
  let text, type, name;
  if (kind === 'csv') {
    const keys = ['_time', 'ls_level', 'ls_severity_number', 'ls_host', 'ls_app', '_msg'];
    const head = keys.join(',');
    const body = state.rows.map((d) => keys.map((k) => `"${String(d[k] ?? '').replace(/"/g, '""')}"`).join(',')).join('\n');
    text = head + '\n' + body; type = 'text/csv'; name = 'search.csv';
  } else {
    text = JSON.stringify(state.rows, null, 2); type = 'application/json'; name = 'search.json';
  }
  download(text, type, `log-server-for-tos-${name}`);
}

function download(text, type, filename) {
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([text], { type }));
  a.download = filename;
  document.body.appendChild(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 5000);
}

/* ── 小工具 ───────────────────────────────────────────────────── */
function toLocalInput(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '';
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
function localTime(iso) {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? String(iso) : d.toLocaleString();
}

function demoRows() {
  return table(['时间 (UTC)', '级别', '主机', '应用', '消息'], [
    ['2026-10-10T04:34:56.789Z', 'err(3)', 'web01', 'sshd', 'Failed password for root from 10.0.0.77'],
  ].map((r) => r.map(esc))) + '<div class="muted" style="font-size:11px;margin-top:6px">（上面是**离线示例**）</div>';
}
