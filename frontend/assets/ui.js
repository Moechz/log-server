/* ui.js —— 组件与迷你图助手（生成 HTML 字符串，零依赖）
 * 所有产出都假定被插入到 #logserver-ui 作用域内（样式依赖该前缀）。
 */

import { icon } from './icons.js?v=__ASSET_VER__';
import { t } from './i18n.js?v=__ASSET_VER__';

/** HTML 转义（所有动态文本必须经过它） */
export function esc(v) {
  return String(v ?? '').replace(/[&<>"']/g, c => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

/* ── 基础片段 ─────────────────────────────────────────────────── */
export const dot = (level = 'off', pulse = false) =>
  `<span class="dot ${esc(level)}${pulse ? ' pulse' : ''}"></span>`;

export const pill = (text, cls = '') =>
  `<span class="pill ${esc(cls)}">${esc(text)}</span>`;

/** 未接入标记：骨架阶段用于明确标注"这里是占位" */
export const todoPill = () => pill(t('common.todo'));

/** 示例数据标记：避免把假数据当成真实数据 */
export const demoPill = () => pill(t('common.demo'), 'accent');

export const kpi = (value, label, unit = '') => `
  <div class="kpi">
    <div class="v">${esc(value)}${unit ? `<small>${esc(unit)}</small>` : ''}</div>
    <div class="k">${esc(label)}</div>
  </div>`;

export const kpis = (list) => `<div class="kpis">${list.map(x => kpi(x.v, x.k, x.unit)).join('')}</div>`;

export const progress = (pct, level = '') => `
  <div class="progress ${esc(level)}"><i style="width:${Math.max(0, Math.min(100, Number(pct) || 0))}%"></i></div>`;

export const kv = (pairs) => `
  <dl class="kv">${pairs.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('')}</dl>`;

export const notice = (text, level = 'info', iconName = null) => `
  <div class="notice ${level === 'info' ? '' : esc(level)}">
    ${icon(iconName || (level === 'crit' ? 'warn' : 'info'), 16)}
    <div>${esc(text)}</div>
  </div>`;

export const empty = (text = null) => `<div class="empty">${esc(text || t('common.empty'))}</div>`;

export const listItem = ({ level = 'off', name, meta, right = '', pulse = false }) => `
  <div class="list-item">
    ${dot(level, pulse)}
    <div style="min-width:0">
      <div class="name">${esc(name)}</div>
      ${meta ? `<div class="meta">${esc(meta)}</div>` : ''}
    </div>
    <div class="right">${right}</div>
  </div>`;

export const table = (headers, rows) => `
  <div class="table-wrap"><table class="table">
    <thead><tr>${headers.map(h => `<th>${esc(h)}</th>`).join('')}</tr></thead>
    <tbody>${rows.map(r => `<tr>${r.map(c => `<td>${c}</td>`).join('')}</tr>`).join('')}</tbody>
  </table></div>`;

/* ── 卡片 / 页面骨架 ──────────────────────────────────────────── */
export const card = ({ title, badge = '', body, cls = '', span = '' }) => `
  <section class="card ${esc(cls)} ${esc(span)}">
    ${(title || badge) ? `<div class="card-head">
      <h3>${esc(title || '')}</h3>
      ${badge}
    </div>` : ''}
    ${body}
  </section>`;

export const pageHead = (title, sub, actions = '') => `
  <div class="page-head">
    <div>
      <h2>${esc(title)}</h2>
      ${sub ? `<p class="sub">${esc(sub)}</p>` : ''}
    </div>
    ${actions ? `<div class="head-actions">${actions}</div>` : ''}
  </div>`;

export const statusbar = (level, stateText, facts = []) => `
  <div class="statusbar ${level === 'ok' ? '' : esc(level)}">
    <div class="state">${dot(level, level === 'crit')}${esc(stateText)}</div>
    <div class="facts">
      ${facts.map(([k, v]) => `<div class="fact"><span class="k">${esc(k)}</span><b>${esc(v)}</b></div>`).join('')}
    </div>
  </div>`;

/* ── 日志面板 ─────────────────────────────────────────────────── */
/** lines: [{ts, level, text}] —— level 用于着色 */
export const logPanel = (lines) => `
  <div class="log-panel">${lines.map(l => {
    const cls = l.level === 'error' ? 'lv-err' : l.level === 'warn' ? 'lv-warn' : '';
    return `<span class="ln"><span class="ts">${esc(l.ts || '')}</span> <span class="${cls}">${esc(l.text)}</span></span>`;
  }).join('')}</div>`;

/* ── 迷你图（自研 SVG，避免引入图表库）────────────────────────── */
/**
 * sparkline(values, {type:'line'|'bar', height})
 * 使用 viewBox="0 0 100 H" + preserveAspectRatio="none" 以自适应宽度。
 */
export function sparkline(values, { type = 'line', height = 38 } = {}) {
  const v = (values || []).map(n => Number(n) || 0);
  if (!v.length) return `<svg class="spark" viewBox="0 0 100 ${height}" preserveAspectRatio="none"></svg>`;

  const max = Math.max(...v, 1);
  const H = height, PAD = 3;
  const x = i => (v.length === 1 ? 50 : (i / (v.length - 1)) * 100);
  const y = n => H - PAD - (n / max) * (H - PAD * 2);

  if (type === 'bar') {
    const w = 100 / v.length;
    const bars = v.map((n, i) => {
      const h = Math.max(0.6, (n / max) * (H - PAD * 2));
      return `<rect class="bar" x="${(i * w + w * 0.15).toFixed(2)}" y="${(H - PAD - h).toFixed(2)}"
              width="${(w * 0.7).toFixed(2)}" height="${h.toFixed(2)}" rx="0.6"/>`;
    }).join('');
    return `<svg class="spark" viewBox="0 0 100 ${H}" preserveAspectRatio="none">${bars}</svg>`;
  }

  const pts = v.map((n, i) => `${x(i).toFixed(2)},${y(n).toFixed(2)}`).join(' ');
  const area = `0,${H} ${pts} 100,${H}`;
  return `<svg class="spark" viewBox="0 0 100 ${H}" preserveAspectRatio="none">
    <polygon class="area" points="${area}"/>
    <polyline class="line" points="${pts}"/>
  </svg>`;
}

/* ── 现代下拉选择器（替代原生 <select>）────────────────────────────
 * 原生 <select> 的弹出列表由浏览器绘制，样式无法与 TOS 控件统一；这里用
 * 自定义按钮 + 浮层列表，视觉与其他控件一致，并保留原生 <select> 的使用姿势：
 *   HTML：  select('id', '默认值', [['value','标签'], ...])
 *   读取：  root.querySelector('#id').value
 *   监听：  root.querySelector('#id').addEventListener('change', fn)
 *   设置：  root.querySelector('#id').setValue('value')
 * 初始化：app.js 在每次渲染后调用 initSelects(scope)（幂等）。
 */
export function select(id, value, options, { width = '', title = '' } = {}) {
  const opts = options.map(([v, l]) => {
    const val = String(v);
    const on = val === String(value);
    return `<button type="button" class="sel-opt${on ? ' is-sel' : ''}" role="option"
        data-value="${esc(val)}" aria-selected="${on}"><span>${esc(l)}</span>
        <svg class="sel-check" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
             stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m5 12.5 4.5 4.5L19 7.5"/></svg>
      </button>`;
  }).join('');
  const cur = options.find(([v]) => String(v) === String(value));
  return `<div class="sel" id="${esc(id)}" data-value="${esc(String(value))}"${title ? ` title="${esc(title)}"` : ''}${width ? ` style="width:${esc(width)}"` : ''}>
    <button type="button" class="sel-btn" aria-haspopup="listbox" aria-expanded="false">
      <span class="sel-label">${esc(cur ? cur[1] : value)}</span>
      <svg class="sel-caret" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
           stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg>
    </button>
    <div class="sel-menu" role="listbox" hidden>${opts}</div>
  </div>`;
}

function selSetValue(el, val) {
  const menu = el.querySelector('.sel-menu');
  const opt = Array.from(menu.querySelectorAll('.sel-opt')).find(o => o.dataset.value === String(val));
  if (!opt) return false;
  el.value = opt.dataset.value;
  el.dataset.value = opt.dataset.value;
  menu.querySelectorAll('.sel-opt').forEach(o => {
    const on = o === opt;
    o.classList.toggle('is-sel', on);
    o.setAttribute('aria-selected', String(on));
  });
  el.querySelector('.sel-label').textContent = opt.querySelector('span').textContent;
  return true;
}

/** 初始化作用域内的下拉组件（设置 .value、绑定交互、键盘可达）。幂等。 */
export function initSelects(scope) {
  const root = scope || document;
  root.querySelectorAll('.sel').forEach((el) => {
    if (el.__selInit) return;
    el.__selInit = true;
    el.value = el.dataset.value;

    const btn = el.querySelector('.sel-btn');
    const menu = el.querySelector('.sel-menu');
    const close = () => {
      menu.hidden = true;
      el.classList.remove('is-open');
      btn.setAttribute('aria-expanded', 'false');
      menu.querySelectorAll('.sel-opt.is-active').forEach(o => o.classList.remove('is-active'));
    };
    const open = () => {
      document.querySelectorAll('.sel.is-open').forEach((o) => {
        if (o === el) return;
        o.classList.remove('is-open');
        const m = o.querySelector('.sel-menu'); if (m) m.hidden = true;
        const b = o.querySelector('.sel-btn'); if (b) b.setAttribute('aria-expanded', 'false');
      });
      menu.hidden = false;
      el.classList.add('is-open');
      btn.setAttribute('aria-expanded', 'true');
      const cur = menu.querySelector('.is-sel');
      if (cur) cur.scrollIntoView({ block: 'nearest' });
    };

    btn.addEventListener('click', (e) => { e.stopPropagation(); menu.hidden ? open() : close(); });
    el.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') { close(); btn.focus(); return; }
      if (e.key === 'Enter' || e.key === ' ') {
        if (document.activeElement === btn) { e.preventDefault(); menu.hidden ? open() : close(); }
        return;
      }
      if (menu.hidden || (e.key !== 'ArrowDown' && e.key !== 'ArrowUp')) return;
      e.preventDefault();
      const opts = Array.from(menu.querySelectorAll('.sel-opt'));
      const ai = opts.findIndex(o => o.classList.contains('is-active'));
      const si = opts.findIndex(o => o.classList.contains('is-sel'));
      let ni = (ai >= 0 ? ai : si) + (e.key === 'ArrowDown' ? 1 : -1);
      ni = Math.max(0, Math.min(opts.length - 1, ni));
      opts.forEach((o, i) => o.classList.toggle('is-active', i === ni));
      opts[ni].scrollIntoView({ block: 'nearest' });
    });
    menu.querySelectorAll('.sel-opt').forEach((opt) => {
      opt.addEventListener('click', (e) => {
        e.stopPropagation();
        selSetValue(el, opt.dataset.value);
        close();
        el.dispatchEvent(new Event('change', { bubbles: true }));
      });
    });
    el.setValue = (v) => selSetValue(el, v);
  });
}

// 点击空白处关闭所有下拉（模块级只注册一次）
if (typeof document !== 'undefined') {
  document.addEventListener('click', () => {
    document.querySelectorAll('.sel.is-open').forEach((el) => {
      el.classList.remove('is-open');
      const m = el.querySelector('.sel-menu'); if (m) m.hidden = true;
      const b = el.querySelector('.sel-btn'); if (b) b.setAttribute('aria-expanded', 'false');
    });
  });
}

/* ── 弹窗（modal）───────────────────────────────────────────────
 * openModal(root, {title, body, wide, onMount}) → 返回 close()
 * 支持：Esc 关闭、点遮罩关闭、右上角关闭按钮。
 */
export function openModal(root, { title, body, wide = false, onMount } = {}) {
  const host = document.getElementById('logserver-ui') || root || document.body;
  const back = document.createElement('div');
  back.className = 'modal-backdrop';
  back.innerHTML = `<div class="modal${wide ? ' wide' : ''}" role="dialog" aria-modal="true">
      <div class="modal-head"><h3>${esc(title)}</h3>
        <button class="icon-btn modal-close" aria-label="关闭">✕</button></div>
      <div class="modal-body">${body}</div>
    </div>`;
  host.appendChild(back);

  const onKey = (e) => { if (e.key === 'Escape') close(); };
  const close = () => {
    document.removeEventListener('keydown', onKey);
    back.remove();
  };
  document.addEventListener('keydown', onKey);
  back.addEventListener('click', (e) => { if (e.target === back) close(); });
  back.querySelector('.modal-close').addEventListener('click', close);
  if (typeof onMount === 'function') onMount(back, close);
  return close;
}

/* ── DOM 助手 ─────────────────────────────────────────────────── */
/** 由 HTML 字符串创建元素（仅取第一个子元素） */
export function el(html) {
  const tpl = document.createElement('template');
  tpl.innerHTML = html.trim();
  return tpl.content.firstElementChild;
}

/** 演示用：生成一段确定性的伪随机序列（避免每次刷新跳变） */
export function demoSeries(n, base = 40, spread = 30, seed = 7) {
  let s = seed;
  const out = [];
  for (let i = 0; i < n; i++) {
    s = (s * 1103515245 + 12345) % 2147483648;
    out.push(Math.round(base + (s / 2147483648) * spread));
  }
  return out;
}
