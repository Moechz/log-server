/* app.js —— 应用骨架：导航、hash 路由、主题、能力徽标、Toast
 *
 * 页面注册表就是"九宫格导航"的唯一来源：新增页面只需在此加一行。
 * 每个页面模块导出：render(root, ctx)  —— 可选 onEnter/onLeave。
 */

import { icon } from './icons.js?v=__ASSET_VER__';
import { t, initLang, setLang, getLang, RTL_LANGS } from './i18n.js?v=__ASSET_VER__';
import * as api from './api.js?v=__ASSET_VER__';
import { esc, initSelects } from './ui.js?v=__ASSET_VER__';
import { maybeStartOnboarding } from './onboarding.js?v=__ASSET_VER__';

// 资源版本（构建时由 packaging/build.sh 注入）——用于给动态 import 加 ?v= 防缓存
const ASSET_VER = '__ASSET_VER__';

/* ── 页面注册表（9 项一级导航，平铺一层）───────────────────────── */
const PAGES = [
  { id: 'overview', key: 'nav.overview', icon: 'overview', mod: './pages/overview.js' },
  { id: 'monitor',  key: 'nav.monitor',  icon: 'monitor',  mod: './pages/monitor.js' },
  { id: 'audit',    key: 'nav.audit',    icon: 'audit',    mod: './pages/audit.js',  needs: 'R2/R3' },
  { id: 'search',   key: 'nav.search',   icon: 'search',   mod: './pages/search.js' },
  { id: 'receive',  key: 'nav.receive',  icon: 'receive',  mod: './pages/receive.js' },
  { id: 'forward',  key: 'nav.forward',  icon: 'forward',  mod: './pages/forward.js' },
  { id: 'storage',  key: 'nav.storage',  icon: 'storage',  mod: './pages/storage.js' },
  { id: 'rules',    key: 'nav.rules',    icon: 'rules',    mod: './pages/rules.js' },
  { id: 'about',    key: 'nav.about',    icon: 'about',    mod: './pages/about.js' },
  { id: 'settings', key: 'nav.settings', icon: 'settings', mod: './pages/settings.js' },
];

/* ── DOM 引用 ─────────────────────────────────────────────────── */
const $root    = document.getElementById('logserver-ui');
const $nav     = document.getElementById('nav');
const $content = document.getElementById('content');
const $toast   = document.getElementById('toast');

let current = null;      // 当前页面注册项
let currentMod = null;   // 当前页面模块

/* ── Toast ────────────────────────────────────────────────────── */
let toastTimer = null;
export function toast(msg, level = 'ok', ms = 2600) {
  $toast.textContent = String(msg);
  $toast.className = 'toast' + (level === 'ok' ? '' : ' ' + level);
  $toast.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { $toast.hidden = true; }, ms);
}

/* ── 主题 ─────────────────────────────────────────────────────── */
function applyTheme(mode) {
  $root.setAttribute('data-theme', mode);
  localStorage.setItem('logserver.theme', mode);
}
function initTheme() {
  const saved = localStorage.getItem('logserver.theme');
  const prefersDark = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
  applyTheme(saved || (prefersDark ? 'dark' : 'light'));
}

/* ── 导航渲染 ─────────────────────────────────────────────────── */
function renderNav() {
  $nav.innerHTML = PAGES.map(p => `
      <a class="nav-item" href="#/${p.id}" data-id="${esc(p.id)}">
        ${icon(p.icon, 16)}<span class="txt">${esc(t(p.key))}</span>
        ${p.needs ? `<span class="pill" title="需要平台能力 ${esc(p.needs)}">${esc(p.needs)}</span>` : ''}
      </a>`).join('');
}

function markActive(id) {
  $nav.querySelectorAll('.nav-item').forEach(a =>
    a.classList.toggle('active', a.dataset.id === id));
}

/* ── 界面语言 ─────────────────────────────────────────────────── */
/** 切换语言：刷新顶栏/导航/当前页，不改变当前所在页面（供「设置」页调用）。 */
function applyLang(code) {
  setLang(code);
  const cur = getLang();
  document.documentElement.lang = cur;
  document.documentElement.dir = RTL_LANGS.includes(cur) ? 'rtl' : 'ltr';
  document.getElementById('app-title').textContent = t('app.title');
  renderNav();
  markActive(current ? current.id : PAGES[0].id);
  mount(parseHash()).then(() => maybeStartOnboarding({ navigate }));
}

/* ── 路由 ─────────────────────────────────────────────────────── */
function parseHash() {
  const raw = (location.hash || '').replace(/^#\/?/, '');
  const [id, qs] = raw.split('?');
  const query = {};
  try {
    new URLSearchParams(qs || '').forEach((v, k) => { query[k] = v; });
  } catch { /* ignore */ }
  return { id: PAGES.some(p => p.id === id) ? id : PAGES[0].id, query };
}

async function mount(route) {
  const { id, query = {} } = route || parseHash();
  const page = PAGES.find(p => p.id === id) || PAGES[0];
  if (currentMod && typeof currentMod.onLeave === 'function') {
    try { currentMod.onLeave(); } catch { /* ignore */ }
  }
  api.abortAll();                       // 取消上一页在途请求
  current = page;
  markActive(page.id);
  document.title = `${t(page.key)} · ${t('app.title')}`;
  $content.innerHTML = `<div class="empty">${esc(t('common.loading'))}</div>`;

  try {
    // 模块缓存：同一页反复切换不重复下载
    currentMod = await import(page.mod + '?v=' + ASSET_VER);
    if (typeof currentMod.render !== 'function') throw new Error('页面模块缺少 render()');
    await currentMod.render($content, { t, api, toast, navigate, esc, icon, page, applyLang,
      query, setTheme: applyTheme, getTheme: () => $root.getAttribute('data-theme') });
    initSelects($content);   // 初始化自定义下拉（.sel）—— 必须在页面渲染完成后
    if (typeof currentMod.onEnter === 'function') currentMod.onEnter();
  } catch (e) {
    $content.innerHTML = `<div class="notice crit">${esc(String(e && e.message || e))}</div>`;
  }
}

export function navigate(id, params) {
  const qs = (params && Object.keys(params).length)
    ? '?' + new URLSearchParams(params).toString() : '';
  location.hash = '#/' + id + qs;
}

/* ── 启动 ─────────────────────────────────────────────────────── */
function boot() {
  const lang = initLang();
  document.documentElement.lang = lang;
  document.documentElement.dir = RTL_LANGS.includes(lang) ? 'rtl' : 'ltr';
  document.getElementById('app-title').textContent = t('app.title');
  initTheme();
  renderNav();

  document.getElementById('refresh-btn').addEventListener('click', () => mount(parseHash()));
  window.addEventListener('hashchange', () => mount(parseHash()));

  mount(parseHash()).then(() => maybeStartOnboarding({ navigate }));
}

boot();

export { PAGES, getLang, setLang };
