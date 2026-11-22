/* api.js —— HTTP 客户端（真实接口优先，未就绪时回退示例数据）
 *
 * 约定：统一响应信封 { ok, data, error }；后端未启动时返回 fallback 并标记 offline，
 * 让前端骨架在任何时候都能渲染（同时明确标注"示例数据"）。
 */

/* ★ 路径前缀推导（修掉一个真 bug）：
 * 应用经平台 nginx 以 /log-server-for-tos/ 前缀访问，且 nginx 模板是
 *   proxy_pass http://127.0.0.1:15280/;   ← 结尾的 / 会**剥离前缀**
 * 所以后端收到的是 /api/v1/...，但浏览器里若请求绝对路径 /api/v1/...
 * 会打到 NAS 根（不是本应用）→ 必须按当前页面路径推导前缀。
 * 直接本地预览（python -m http.server，页面在 /）时前缀为 /，同样正确。
 */
const APP_PREFIX = (() => {
  const p = location.pathname || '/';
  const i = p.lastIndexOf('/');
  return p.slice(0, i + 1) || '/';
})();
const BASE = APP_PREFIX + 'api/v1';
const DEFAULT_TIMEOUT = 12000;

/* TOS 会话（官方文档 §8.10）：
 *  - External Open（我们的形态，D-018）下页面与 TOS 同源，浏览器会自动携带
 *    TMSESSNAME / X-Csrf-Token cookie；
 *  - 官方同时约定可用自定义 `Cookie` 头显式传递（Internal Open 场景），
 *    我们一并附上，两种打开方式都能认证。
 */
function getCookie(name) {
  const prefix = encodeURIComponent(name) + '=';
  const hit = (document.cookie || '').split(';').map(s => s.trim()).find(s => s.startsWith(prefix));
  return hit ? decodeURIComponent(hit.slice(prefix.length)) : '';
}

const ADMIN_TOKEN_KEY = 'logserver.adminToken';

/** 读取/保存本机浏览器会话里的管理员令牌（只存 sessionStorage，关标签页即失效）。 */
export function getAdminToken() {
  try { return sessionStorage.getItem(ADMIN_TOKEN_KEY) || ''; } catch { return ''; }
}
export function setAdminToken(token) {
  try {
    if (token) sessionStorage.setItem(ADMIN_TOKEN_KEY, token);
    else sessionStorage.removeItem(ADMIN_TOKEN_KEY);
  } catch { /* 隐私模式等：忽略，仍可只读使用 */ }
}

/** 管理员令牌头（跨站请求无法自定义该头，因此也天然防 CSRF）。 */
export function adminHeaders() {
  const t = getAdminToken();
  return t ? { 'X-Logserver-Admin-Token': t } : {};
}

function authHeaders() {
  const h = { ...adminHeaders() };
  const session = getCookie('TMSESSNAME');
  const csrf = getCookie('X-Csrf-Token');
  if (csrf) h['X-Csrf-Token'] = csrf;
  if (session) h['Cookie'] = `TMSESSNAME=${session}; X-Csrf-Token=${csrf};`;
  return h;
}

/** 会话失效时跳回 TOS 登录页（官方 §8.10：不要自动刷新 token） */
let redirecting = false;
function onUnauthorized() {
  if (redirecting) return;
  redirecting = true;
  location.href = '/';
}

export const appPrefix = APP_PREFIX;

const inflight = new Set();
export function abortAll() {
  for (const c of inflight) { try { c.abort(); } catch { /* ignore */ } }
  inflight.clear();
}

async function request(path, { method = 'GET', body, timeout = DEFAULT_TIMEOUT, fallback } = {}) {
  const ctrl = new AbortController();
  inflight.add(ctrl);
  const timer = setTimeout(() => ctrl.abort(), timeout);
  try {
    const headers = { ...authHeaders() };
    if (body) headers['Content-Type'] = 'application/json';
    const res = await fetch(path.startsWith('http') ? path : BASE + path, {
      method,
      headers,
      body: body ? JSON.stringify(body) : undefined,
      signal: ctrl.signal,
      credentials: 'include',
    });
    if (res.status === 401) {
      // 会话失效：跳回 `/` 触发 TOS 重新登录（官方 §8.10）
      onUnauthorized();
      return { ok: false, offline: false, unauthorized: true,
               error: { code: 'unauthorized', message: '会话已过期，正在跳转登录' } };
    }
    const text = await res.text();
    let json = null;
    try { json = text ? JSON.parse(text) : null; } catch { /* 非 JSON */ }
    if (!res.ok) {
      return { ok: false, offline: false, error: (json && json.error) || { code: res.status, message: res.statusText } };
    }
    return { ok: true, offline: false, data: json && 'data' in json ? json.data : json };
  } catch (e) {
    if (fallback !== undefined) return { ok: true, offline: true, data: fallback };
    return { ok: false, offline: true, error: { code: 'offline', message: String((e && e.message) || e) } };
  } finally {
    clearTimeout(timer);
    inflight.delete(ctrl);
  }
}

export const get = (p, o) => request(p, { ...o, method: 'GET' });
export const post = (p, body, o) => request(p, { ...o, method: 'POST', body });
export const put = (p, body, o) => request(p, { ...o, method: 'PUT', body });
export const del = (p, o) => request(p, { ...o, method: 'DELETE' });

// ── 各页面数据源（全部带 fallback，便于后端未就绪时预览）──────────────
export const about = (fb) => get('/about', { fallback: fb });
export const capabilities = (fb) => get('/capabilities', { fallback: fb });
export const status = (fb) => get('/status', { fallback: fb });
export const network = (fb) => get('/network', { fallback: fb });
export const overview = (fb) => get('/overview', { fallback: fb });
export const sources = (fb) => get('/sources', { fallback: fb });
export const receivePolicy = (fb) => get('/receive/policy', { fallback: fb });
export const putReceivePolicy = (body) => put('/receive/policy', body);
export const putSources = (sources) => put('/sources', { sources });
export const pauseIngest = () => post('/ingest/pause');
export const resumeIngest = () => post('/ingest/resume');
export const forwardTargets = (fb) => get('/forward/targets', { fallback: fb });
export const putForwardTargets = (targets) => put('/forward/targets', { targets });
export const testForwardTarget = (id) => post(`/forward/targets/${id}/test`);
export const alerts = (n = 50, fb) => get(`/alerts?n=${n}`, { fallback: fb });
export const alertAck = (id) => post(`/alerts/${encodeURIComponent(id)}/ack`);
export const alertAckAll = () => post('/alerts/ack-all');
export const alertSilences = (fb) => get('/alerts/silences', { fallback: fb });
export const alertSilenceCreate = (body) => post('/alerts/silences', body);
export const alertSilenceDelete = (id) => del(`/alerts/silences/${encodeURIComponent(id)}`);
export const alertMaintenanceSet = (body) => post('/alerts/maintenance', body);
export const alertMaintenanceClear = () => del('/alerts/maintenance');
// FR-803 通知通道
export const notifyChannels = (fb) => get('/notify/channels', { fallback: fb });
export const putNotifyChannels = (channels) => put('/notify/channels', { channels });
export const testNotify = (kind) => post('/notify/test', { kind });
export const rules = (fb) => get('/rules', { fallback: fb });
export const auditActions = (n = 100) => get('/audit/actions?limit=' + n);
export const onboarding = () => get('/onboarding');
export const completeOnboarding = () => post('/onboarding/complete');
export const resetOnboarding = () => post('/onboarding/reset');
export const dryRunRules = (samples) => post('/rules/dryrun', { samples });
export const storageUsage = (fb) => get('/storage/usage', { fallback: fb });
export const estimate = (body) => post('/storage/estimate', body);
export const storageLocation = (fb) => get('/storage/location', { fallback: fb });
export const storageBrowse = (path) => get('/storage/browse' + (path ? '?path=' + encodeURIComponent(path) : ''));
export const putStorageLocation = (engineDir) => put('/storage/location', { engine_dir: engineDir });
export const logs = (params, fb) => get('/logs?' + new URLSearchParams(params).toString(), { fallback: fb });
export const logsHistogram = (params) => get('/logs/histogram?' + new URLSearchParams(params).toString());
export const logsContext = (params) => get('/logs/context?' + new URLSearchParams(params).toString());
export const selflog = (n = 100, fb) => get(`/selflog?n=${n}`, { fallback: fb });
export const metrics = () => get('/metrics');
export const session = (fb) => get('/session', { fallback: fb });
export const diagnostics = () => get('/diagnostics');
