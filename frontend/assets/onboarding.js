/* onboarding.js —— 首次使用向导（FR-1005）
 *
 * 触发：管理员登录后、onboarding.json 未标记完成、本标签页未选择"稍后"。
 * 四步：保留/存储（只读说明）→ 采集源（可添加一个文件源）→ 告警通道（说明）→ 转发（可选）。
 * 不重复页面表单：第 3/4 步给出配置入口说明，避免维护两套表单。
 */
import { openModal, esc, notice, kv } from './ui.js';
import * as api from './api.js';

const SNOOZE_KEY = 'logserver.onboarding.snooze';

export async function maybeStartOnboarding({ navigate } = {}) {
  try {
    if (sessionStorage.getItem(SNOOZE_KEY) === '1') return;
    const ses = await api.session();
    if (!ses.ok || !ses.data || !ses.data.user || !ses.data.user.is_admin) return;
    if (ses.data.user.source === 'dev') return; // 本地开发不弹
    const st = await api.onboarding();
    if (!st.ok || !st.data || !st.data.state || st.data.state.completed) return;
    show(st.data, navigate);
  } catch { /* 向导失败绝不阻塞应用 */ }
}

function show(data, navigate) {
  let step = 1;
  const host = document.getElementById('logserver-ui') || document.body;

  openModal(host, {
    title: '首次使用向导',
    wide: true,
    body: '<div id="ob-body"></div>',
    onMount: (back, close) => {
      const body = back.querySelector('#ob-body');
      const draw = () => {
        body.innerHTML = stepHTML(step, data);
        bind(body, close);
      };
      const bind = (box, close) => {
        const msg = (text, kind) => {
          const el = box.querySelector('#ob-msg');
          if (el) { el.textContent = text; el.style.color = kind === 'err' ? 'var(--danger)' : 'var(--success)'; }
        };
        const prev = box.querySelector('[data-prev]');
        if (prev) prev.addEventListener('click', () => { if (step > 1) { step--; draw(); } });
        const next = box.querySelector('[data-next]');
        if (next) next.addEventListener('click', async () => {
          if (step === 2) {
            const path = (box.querySelector('#ob-path').value || '').trim();
            if (path) {
              if (!path.startsWith('/')) { msg('采集路径必须是绝对路径', 'err'); return; }
              const cur = await api.sources();
              const existing = (cur.ok && cur.data && cur.data.items ? cur.data.items : [])
                .filter(x => x.kind !== 'journald')
                .map(x => ({ path: x.path, enabled: x.enabled !== false, multiline: !!x.multiline, encoding: x.encoding || '' }));
              if (existing.some(x => x.path === path)) { msg('该路径已在采集列表中', 'err'); return; }
              const ml = box.querySelector('#ob-ml');
              const r = await api.putSources([...existing, { path, enabled: true, multiline: !!(ml && ml.checked), encoding: '' }]);
              if (!r.ok) { msg((r.error && r.error.message) || '添加失败', 'err'); return; }
            }
          }
          if (step < 4) { step++; draw(); }
        });
        const finish = async () => {
          const r = await api.completeOnboarding();
          if (r.ok) close();
          else msg((r.error && r.error.message) || '保存失败', 'err');
        };
        const fin = box.querySelector('[data-finish]');
        if (fin) fin.addEventListener('click', finish);
        const skip = box.querySelector('[data-skip]');
        if (skip) skip.addEventListener('click', async () => {
          const r = await api.completeOnboarding();
          if (r.ok) close();
          else { sessionStorage.setItem(SNOOZE_KEY, '1'); close(); }
        });
        const later = box.querySelector('[data-later]');
        if (later) later.addEventListener('click', () => {
          sessionStorage.setItem(SNOOZE_KEY, '1');
          close();
        });
        const goRules = box.querySelector('[data-go-rules]');
        if (goRules) goRules.addEventListener('click', () => {
          sessionStorage.setItem(SNOOZE_KEY, '1');
          close();
          if (typeof navigate === 'function') navigate('rules');
        });
        const goFwd = box.querySelector('[data-go-forward]');
        if (goFwd) goFwd.addEventListener('click', () => {
          sessionStorage.setItem(SNOOZE_KEY, '1');
          close();
          if (typeof navigate === 'function') navigate('forward');
        });
      };
      draw();
    },
  });
}

function stepHTML(step, data) {
  const hints = data.hints || {};
  const head = `<div class="muted" style="font-size:12px;margin-bottom:10px">
      第 <b>${step}</b> / 4 步 · ${['保留与存储', '采集源', '告警通道', '转发目标'][step - 1]}
    </div>`;
  const foot = (nextLabel) => `<div style="display:flex;justify-content:space-between;align-items:center;gap:8px;margin-top:14px;flex-wrap:wrap">
      <span class="btn" data-skip>跳过向导</span>
      <span style="display:flex;gap:8px;align-items:center">
        <span class="btn" data-prev ${step === 1 ? 'style="display:none"' : ''}>上一步</span>
        ${step === 4 ? '<span class="btn primary" data-finish>完成</span>' : `<span class="btn primary" data-next>${esc(nextLabel)}</span>`}
      </span>
    </div>
    <div class="mono" id="ob-msg" style="font-size:12px;margin-top:8px;min-height:16px"></div>`;

  if (step === 1) {
    return head + kv([
      ['全局保留（引擎）', `${data.retention_days || 14} 天`],
      ['数据目录', data.data_dir || '—'],
      ['存储引擎', 'VictoriaLogs（内置）'],
    ]) + notice('当前版本只有**全局保留**；按分类分档（ops/security/hardware）尚未实现（D-033）。可在「存储」页查看容量与位置。', 'info', 'info') + foot('下一步');
  }
  if (step === 2) {
    return head + `
      <div class="muted" style="font-size:12px;margin-bottom:8px">
        已配置文件源：<b>${Number(hints.file_sources || 0)}</b> 个。可选：现在添加一个日志文件路径（绝对路径）。
      </div>
      <label style="font-size:var(--font-table)">文件路径（留空 = 跳过）
        <input id="ob-path" class="field mono" placeholder="/var/log/nginx/access.log">
      </label>
      <label style="display:flex;align-items:center;gap:8px;margin-top:8px;font-size:12px">
        <input id="ob-ml" type="checkbox"> 多行合并（Java 堆栈等）
      </label>
      <div class="muted" style="font-size:11px;margin-top:8px">系统日志（journald）需要平台授权 R1；无权限时会在「接收」页如实显示未采集，不会自我授权。</div>
      ${foot('添加并继续')}`;
  }
  if (step === 3) {
    return head + `
      <div class="muted" style="font-size:12px;margin-bottom:8px">
        已配置通知通道：<b>${Number(hints.notify_channels || 0)}</b> 个。告警可发到 SMTP 邮件或 Webhook。
        TOS 通知中心需官方注入方法（R8），本版不接入（D-027）。
      </div>
      <div style="display:flex;gap:8px;flex-wrap:wrap">
        <span class="btn" data-go-rules>去「规则」页配置通知</span>
        <span class="btn" data-later>稍后再配</span>
      </div>
      ${foot('下一步')}`;
  }
  return head + `
    <div class="muted" style="font-size:12px;margin-bottom:8px">
      已配置转发目标：<b>${Number(hints.forward_targets || 0)}</b> 个。转发是可选功能：把日志实时送到 SIEM / 另一台日志服务器。
    </div>
    <div style="display:flex;gap:8px;flex-wrap:wrap">
      <span class="btn" data-go-forward>去「转发」页添加目标</span>
    </div>
    ${foot('完成')}`;
}
