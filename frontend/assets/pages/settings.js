/* pages/settings.js —— 设置（界面偏好）
 * 目前承接：界面语言（原在顶栏，按要求移入侧栏「设置」）。
 * 默认跟随 TOS 当前语言；用户显式选择后保存在本机浏览器。
 */
import { card, pageHead, select } from '../ui.js?v=__ASSET_VER__';
import { t, getLang, LANGUAGES } from '../i18n.js?v=__ASSET_VER__';
import * as api from '../api.js?v=__ASSET_VER__';
import { appPrefix, adminHeaders, setAdminToken } from '../api.js?v=__ASSET_VER__';

export function render(root, ctx) {
  const cur = getLang();
  const theme = (typeof ctx.getTheme === 'function' ? ctx.getTheme() : 'light') || 'light';

  root.innerHTML = [
    pageHead(t('nav.settings'), '界面语言与外观'),

    '<div class="grid wide">',

    card({
      title: '界面语言',
      body: `
        <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">
          <label>显示语言</label>
          ${select('set-lang', cur, LANGUAGES.map(l => [l.code, l.label]), { width: '260px' })}
        </div>
        <div class="muted" style="font-size:12px;margin-top:10px">
          共 <b>23</b> 种语言（跟随 TOS 语言体系）。默认跟随 TOS 当前语言；切换后界面立即生效，选择保存在本机浏览器。
          简体中文 / 繁體中文 / English 为完整译文，其余语言未提供的界面文字会<b>回退英文</b>。
        </div>`,
    }),

    card({
      title: '外观',
      body: `
        <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">
          <label>深浅色</label>
          ${select('set-theme', theme, [['light', '浅色'], ['dark', '深色']], { width: '160px' })}
        </div>
        <div class="muted" style="font-size:12px;margin-top:10px">
          主题与语言都是本机浏览器偏好，不影响其他用户。
        </div>`,
    }),

    card({
      title: '管理员权限',
      body: `
        <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">
          <span>当前角色</span><span id="adm-role" class="pill">读取中…</span>
        </div>
        <div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap;margin-top:10px">
          <input id="adm-token" class="field mono" type="password" placeholder="粘贴管理员令牌"
                 autocomplete="off" style="min-width:280px">
          <span class="btn" id="adm-claim" style="display:none">成为管理员（首次）</span>
          <span class="btn" id="adm-save">解锁</span>
          <span class="btn" id="adm-clear">清除</span>
          <span class="mono" id="adm-msg" style="font-size:12px"></span>
        </div>
        <div class="muted" style="font-size:12px;margin-top:10px">
          默认所有 TOS 登录用户为 <b>operator</b>（检索、告警确认、暂停采集）；<b>修改配置、存储位置、导出配置</b>
          需要管理员。令牌文件：<span class="mono">&lt;数据目录&gt;/config/admin.token</span>（0600，首次启动生成），
          用 NAS 管理员读取后粘贴。令牌只保存在当前标签页（关闭即失效），不会写入服务端配置。
        </div>`,
    }),

    card({
      title: '首次使用向导',
      body: `
        <div style="display:flex;gap:8px;align-items:center;flex-wrap:wrap">
          <span class="btn" id="ob-reset">重新运行向导</span>
          <span class="mono" id="ob-msg" style="font-size:12px"></span>
        </div>
        <div class="muted" style="font-size:12px;margin-top:10px">
          清空“向导已完成”标记，下次打开应用时重新显示四步引导（仅管理员；不修改任何配置）。
        </div>`,
    }),

    card({
      title: '配置备份（导出 / 导入）',
      body: `
        <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">
          <span class="btn" id="cfg-export">导出配置</span>
          <label class="btn" for="cfg-import-file" style="cursor:pointer">导入配置</label>
          <input type="file" id="cfg-import-file" accept="application/json,.json" style="display:none">
          <span class="mono" id="cfg-msg" style="font-size:12px"></span>
        </div>
        <div class="muted" style="font-size:12px;margin-top:10px">
          导出内容：采集源、转发目标、告警规则、存储位置（JSON）。<b>不含日志数据，也不含 TLS 私钥</b>。
          导入后<b>需重启应用</b>才生效。<br>
          重要：TOS 的「系统备份 / 换机迁移 / 快照」<b>不覆盖第三方应用数据</b>（已确认），
          因此换机或重装前请先用这里导出配置；需要连日志一起保留时，请直接备份数据目录
          <span class="mono">/Volume1/@apps/log-server-for-tos/data/</span>（建议先停止应用以保证写入一致）。
        </div>`,
    }),

    '</div>',
  ].join('');

  const msg = (text, kind) => {
    const el = root.querySelector('#cfg-msg');
    if (el) { el.textContent = text; el.style.color = kind === 'err' ? '#c0392b' : '#2e7d32'; }
  };

  const exp = root.querySelector('#cfg-export');
  if (exp) {
    exp.addEventListener('click', async () => {
      msg('导出中…');
      try {
        const res = await fetch(appPrefix + 'api/v1/config/export', { credentials: 'include', headers: { ...adminHeaders() } });
        if (!res.ok) {
          const j = await res.json().catch(() => null);
          throw new Error((j && j.error && j.error.message) || ('HTTP ' + res.status));
        }
        const blob = await res.blob();
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        const cd = res.headers.get('Content-Disposition') || '';
        const m = /filename="?([^";]+)"?/.exec(cd);
        a.href = url;
        a.download = m ? m[1] : 'log-server-for-tos-config.json';
        document.body.appendChild(a); a.click(); a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 4000);
        msg('已导出');
      } catch (e) {
        msg('导出失败：' + String((e && e.message) || e), 'err');
      }
    });
  }

  const imp = root.querySelector('#cfg-import-file');
  if (imp) {
    imp.addEventListener('change', async () => {
      const f = imp.files && imp.files[0];
      if (!f) return;
      msg('导入中…');
      try {
        const text = await f.text();
        const res = await fetch(appPrefix + 'api/v1/config/import', {
          method: 'POST', credentials: 'include',
          headers: { 'Content-Type': 'application/json', ...adminHeaders() }, body: text,
        });
        const j = await res.json().catch(() => null);
        if (!res.ok) throw new Error((j && j.error && j.error.message) || ('HTTP ' + res.status));
        msg('已导入，重启应用后生效');
      } catch (e) {
        msg('导入失败：' + String((e && e.message) || e), 'err');
      } finally {
        imp.value = '';
      }
    });
  }

  // ── 管理员解锁（D-032）────────────────────────────────────────────
  const roleEl = root.querySelector('#adm-role');
  const admMsg = (text, kind) => {
    const el = root.querySelector('#adm-msg');
    if (el) { el.textContent = text; el.style.color = kind === 'err' ? '#c0392b' : '#2e7d32'; }
  };
  const refreshRole = async () => {
    if (!roleEl) return;
    const r = await api.session();
    if (r.ok && r.data && r.data.user) {
      const u = r.data.user;
      roleEl.textContent = u.is_admin ? `${u.role}（管理员）` : String(u.role || 'operator');
      roleEl.className = 'pill ' + (u.is_admin ? 'ok' : '');
      const claimBtn = root.querySelector('#adm-claim');
      if (claimBtn) {
        claimBtn.style.display = (r.data.auth && r.data.auth.admin_claimable && !u.is_admin) ? '' : 'none';
      }
      if (r.data.auth && r.data.auth.admin_token === false) {
        admMsg('管理员令牌未启用（检查数据目录可写性）', 'err');
      }
    } else {
      roleEl.textContent = '离线/未登录';
    }
  };
  refreshRole();

  const saveBtn = root.querySelector('#adm-save');
  if (saveBtn) {
    saveBtn.addEventListener('click', async () => {
      const input = root.querySelector('#adm-token');
      const tok = (input && input.value || '').trim();
      if (!tok) { admMsg('请粘贴管理员令牌', 'err'); return; }
      setAdminToken(tok);
      const r = await api.session();
      if (r.ok && r.data && r.data.user && r.data.user.is_admin) {
        admMsg('已解锁管理员，正在刷新…');
        setTimeout(() => location.reload(), 600);
      } else {
        admMsg('令牌无效（未获得管理员角色）', 'err');
      }
    });
  }
  const claimBtn = root.querySelector('#adm-claim');
  if (claimBtn) {
    claimBtn.addEventListener('click', async () => {
      const r = await api.post('/admin/claim');
      if (r.ok) {
        admMsg('已认领为管理员，正在刷新…');
        setTimeout(() => location.reload(), 600);
      } else {
        admMsg((r.error && r.error.message) || '认领失败', 'err');
        refreshRole();
      }
    });
  }
  const clearBtn = root.querySelector('#adm-clear');
  if (clearBtn) {
    clearBtn.addEventListener('click', () => {
      setAdminToken('');
      const input = root.querySelector('#adm-token');
      if (input) input.value = '';
      admMsg('已清除本标签页的管理员令牌');
      refreshRole();
    });
  }

  const obBtn = root.querySelector('#ob-reset');
  if (obBtn) {
    obBtn.addEventListener('click', async () => {
      const el = root.querySelector('#ob-msg');
      const r = await api.resetOnboarding();
      if (!r.ok) { if (el) el.textContent = '❌ ' + ((r.error && r.error.message) || '重置失败'); return; }
      if (el) el.textContent = '已重置，正在刷新…';
      setTimeout(() => location.reload(), 500);
    });
  }

  const sel = root.querySelector('#set-lang');
  if (sel && typeof ctx.applyLang === 'function') {
    sel.addEventListener('change', () => ctx.applyLang(sel.value));
  }
  const themeSel = root.querySelector('#set-theme');
  if (themeSel && typeof ctx.setTheme === 'function') {
    themeSel.addEventListener('change', () => ctx.setTheme(themeSel.value));
  }
}
