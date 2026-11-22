/* i18n.js —— 轻量多语言（零依赖）
 * 约定：
 *   - 默认 zh-CN；支持 zh-HK / en
 *   - 文案随 TOS 语言包（FR-1212）：至少覆盖简中/繁中/英
 *   - 未命中的 key 回退到 key 本身（便于开发期发现漏译）
 */

const dict = {
  'zh-cn': {
    'app.title': 'log server for TOS',
    'nav.overview': '总览',
    'nav.monitor': '监控',
    'nav.audit': '审计',
    'nav.search': '检索',
    'nav.receive': '接收',
    'nav.forward': '转发',
    'nav.storage': '存储',
    'nav.rules': '规则',
    'nav.about': '关于',
    'nav.settings': '设置',
    'common.loading': '加载中…',
    'common.empty': '暂无数据',
    'common.todo': '待接入',
    'common.demo': '示例数据',
    'common.refresh': '刷新',
    'common.save': '保存',
    'common.cancel': '取消',
    'common.ok': '正常',
    'common.warn': '注意',
    'common.crit': '异常',
    'common.unknown': '未知',
    'common.enabled': '已启用',
    'common.disabled': '已停用',
    'cap.checking': '探测中…',
    'cap.all_ok': '平台能力就绪',
    'cap.partial': '部分能力不可用',
    'cap.missing': '平台能力不可用',
    'offline': '未连接到后端（当前显示示例数据）',
  },
  'zh-hk': {
    'app.title': 'log server for TOS',
    'nav.overview': '總覽',
    'nav.monitor': '監控',
    'nav.audit': '審計',
    'nav.search': '檢索',
    'nav.receive': '接收',
    'nav.forward': '轉發',
    'nav.storage': '儲存',
    'nav.rules': '規則',
    'nav.about': '關於',
    'nav.settings': '設定',
    'common.todo': '待接入',
    'common.demo': '示例數據',
    'cap.checking': '探測中…',
    'offline': '未連接到後端（目前顯示示例數據）',
  },
  'en-us': {
    'app.title': 'log server for TOS',
    'nav.overview': 'Overview',
    'nav.monitor': 'Monitoring',
    'nav.audit': 'Audit',
    'nav.search': 'Search',
    'nav.receive': 'Receive',
    'nav.forward': 'Forward',
    'nav.storage': 'Storage',
    'nav.rules': 'Rules',
    'nav.about': 'About',
    'nav.settings': 'Settings',
    'common.loading': 'Loading…',
    'common.empty': 'No data',
    'common.todo': 'Not wired',
    'common.demo': 'Sample data',
    'common.refresh': 'Refresh',
    'common.save': 'Save',
    'common.cancel': 'Cancel',
    'common.ok': 'Healthy',
    'common.warn': 'Warning',
    'common.crit': 'Critical',
    'common.unknown': 'Unknown',
    'common.enabled': 'Enabled',
    'common.disabled': 'Disabled',
    'cap.checking': 'Probing…',
    'cap.all_ok': 'Platform capabilities ready',
    'cap.partial': 'Some capabilities unavailable',
    'cap.missing': 'Capabilities unavailable',
    'offline': 'Backend not connected (showing sample data)',
  },
};

let lang = 'zh-cn';

/*
 * 界面语言（NFR-C2：跟随平台 23 语体系）。
 *   code  = TOS 的语言代码（与 localStorage['langModel'] / .lang 一致）
 *   label = 语言自称（跨语言可读）
 *   full  = 本应用是否已有完整界面翻译；false 的条目回退英文
 *           （与项目 .lang 的做法一致：23 个语段，未译处用英文）
 */
export const LANGUAGES = [
  { code: 'en-us', label: 'English',          full: true },
  { code: 'zh-cn', label: '简体中文',           full: true },
  { code: 'zh-hk', label: '繁體中文',           full: true },
  { code: 'fr-fr', label: 'Français',          full: false },
  { code: 'de-de', label: 'Deutsch',           full: false },
  { code: 'it-it', label: 'Italiano',          full: false },
  { code: 'es-es', label: 'Español',           full: false },
  { code: 'hu-hu', label: 'Magyar',            full: false },
  { code: 'ja-jp', label: '日本語',             full: false },
  { code: 'ko-kr', label: '한국어',             full: false },
  { code: 'pl-pl', label: 'Polski',            full: false },
  { code: 'ru-ru', label: 'Русский',           full: false },
  { code: 'tr-tr', label: 'Türkçe',            full: false },
  { code: 'pt-pt', label: 'Português',         full: false },
  { code: 'ar-sa', label: 'العربية',            full: false },
  { code: 'cs-cz', label: 'Čeština',           full: false },
  { code: 'he-il', label: 'עברית',              full: false },
  { code: 'id-id', label: 'Bahasa Indonesia',  full: false },
  { code: 'nb-no', label: 'Norsk bokmål',      full: false },
  { code: 'nl-nl', label: 'Nederlands',        full: false },
  { code: 'sv-se', label: 'Svenska',           full: false },
  { code: 'th-th', label: 'ไทย',               full: false },
  { code: 'vi-vn', label: 'Tiếng Việt',        full: false },
];

/** 右到左书写的语言（需在 <html> 上设 dir=rtl）。 */
export const RTL_LANGS = ['ar-sa', 'he-il'];

const has = (code) => LANGUAGES.some(l => l.code === code);

/** 归一化 TOS / 浏览器语言代码（TOS 的 langModel 可能带引号或大写）。 */
function normLang(raw) {
  if (!raw) return '';
  let v = String(raw).trim();
  if (v.startsWith('"') && v.endsWith('"')) {
    try { v = JSON.parse(v); } catch { /* 保持原值 */ }
  }
  return String(v).toLowerCase().replace('_', '-');
}

/** 把任意语言代码映射到本应用的 23 语之一（未命中回退英文）。 */
function mapToKnown(code) {
  if (!code) return 'en-us';
  if (has(code)) return code;
  if (code === 'zh-tw' || code === 'zh-mo' || /^zh-hant/.test(code)) return 'zh-hk';
  if (code.startsWith('zh')) return 'zh-cn';
  if (code.startsWith('en')) return 'en-us';
  const base = code.split('-')[0];
  const hit = LANGUAGES.find(l => l.code.split('-')[0] === base);
  return hit ? hit.code : 'en-us';
}

/**
 * 探测默认语言：优先 TOS 当前语言，其次浏览器语言，最后回退英文。
 * 应用以 External Open（D-018）与 TOS 同源，因此可直接读 TOS 写入的
 * localStorage['langModel']（TOS 7 维护的语言键）。
 */
export function detectLang() {
  let code = '';
  try { code = normLang(localStorage.getItem('langModel')); } catch { /* 隐私模式 */ }
  if (!code) code = normLang(navigator.language || '');
  return mapToKnown(code);
}

/** 旧版内部的 3 语代码 → TOS 代码（平滑迁移已有选择）。 */
const LEGACY = { 'zh-CN': 'zh-cn', 'zh-HK': 'zh-hk', 'en': 'en-us' };

/** 探测初始语言：优先用户在本应用内的显式选择，其次跟随 TOS / 浏览器。 */
export function initLang() {
  const saved = localStorage.getItem('logserver.lang');
  if (saved) {
    const code = LEGACY[saved] || (has(saved) ? saved : '');
    if (code) { lang = code; localStorage.setItem('logserver.lang', code); return lang; }
  }
  lang = detectLang();
  return lang;
}

export function setLang(l) {
  const code = LEGACY[l] || (has(l) ? l : '');
  if (!code) return;
  lang = code;
  localStorage.setItem('logserver.lang', code);
}
export function getLang() { return lang; }

/** 翻译：选中语言 → 英文 → 简体中文 → key（未译部分回退英文，与 .lang 口径一致）。 */
export function t(key, ...args) {
  const s = (dict[lang] && dict[lang][key])
    || (dict['en-us'] && dict['en-us'][key])
    || (dict['zh-cn'] && dict['zh-cn'][key])
    || key;
  return args.length ? s.replace(/\{(\d+)\}/g, (m, i) => (args[+i] ?? m)) : s;
}
