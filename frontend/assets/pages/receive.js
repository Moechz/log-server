/* pages/receive.js —— 接收（监听器 + 采集管理 + journald）
 * FR-102：文件采集源可在 UI 增删/启停（落 config.json 并热生效）；
 * FR-1107：采集总开关（维护模式）——暂停读取（文件+journald），接收端继续收并写入存储；
 * FR-602： 「帮助」弹窗告诉发送方“发到哪个地址/端口”。
 */
import { card, kpis, kv, listItem, table, notice, demoPill, pill, pageHead, esc, openModal } from '../ui.js?v=__ASSET_VER__';
import { t } from '../i18n.js?v=__ASSET_VER__';
import * as api from '../api.js?v=__ASSET_VER__';

const MATRIX = [
  ['UDP syslog', '514', '✅ M1', '低（会丢、超 MTU 截断）', '交换机 / 防火墙 / 路由器'],
  ['TCP syslog', '514', '✅ M1', '中（断连期间丢；需磁盘队列）', 'Linux / 网络设备'],
  ['TLS syslog', '6514', '🔜 待证书', '中高', '跨公网'],
  ['HTTP bulk', '自定义', '✅ 引擎侧支持', '中高', 'Loki / ES / OpenObserve'],
  ['RELP / GELF', '20514 / 12201', 'P1', '高 / 中', 'rsyslog / Graylog'],
  ['SNMP Trap', '15162', 'P1', '低', '网络设备告警（162 被系统占用）'],
];

// 可编辑的文件采集源（模块级，保证重新渲染时不丢未保存的编辑）
let editRows = [];
let editInit = false;

export async function render(root) {
  const [or, sr, nr, pr] = await Promise.all([api.overview(null), api.sources(null), api.network(null), api.receivePolicy(null)]);
  const offline = or.offline || !or.ok;
  const rx = (!offline && or.data.rx) || {};
  const listeners = rx.listeners || [];
  const allSources = (!sr.offline && sr.ok && sr.data.items) || [];
  const paused = (!sr.offline && sr.ok && !!sr.data.paused) || false;
  const jrn = allSources.find(s2 => s2.kind === 'journald') || null;
  const fileSources = allSources.filter(s2 => s2.kind !== 'journald');
  const statusByPath = {};
  fileSources.forEach(st => { statusByPath[st.path] = st; });

  // 首次进入本页时，把服务器上的文件源灌进可编辑列表
  if (!sr.offline && sr.ok && !editInit) {
    editRows = fileSources.map(s2 => ({
      path: s2.path, enabled: s2.enabled !== false, multiline: !!s2.multiline, encoding: s2.encoding || '',
    }));
    editInit = true;
  }

  // 发送方地址（帮助弹窗用）
  const net = (!nr.offline && nr.ok && nr.data) || {};
  const pol = (!pr.offline && pr.ok && pr.data && pr.data.policy) || {};
  const ips = (net.addresses || []).map(a => ({ ip: a.ip, iface: a.iface }));
  const hostname = net.hostname || location.hostname;
  const primary = (ips[0] && ips[0].ip) || location.hostname;
  const seen = new Set();
  const addrs = [[hostname, '主机名'], ...ips.map(a => [a.ip, a.iface])]
    .filter(([v]) => v && !seen.has(v) && seen.add(v));
  const portOf = (proto) => {
    const l = listeners.find(x => x.protocol === proto);
    return l ? { port: l.port, enabled: !!l.enabled } : { port: null, enabled: false };
  };
  const helpHTML = helpBody(addrs, primary, portOf('udp'), portOf('tcp'), portOf('tls'));

  // 卡头徒标反映“实际在干什么”，而不是笼统的“运行中”：
  //   暂停中 → 已暂停；有文件源 → 运行中 · N 个文件源；无文件源 → 未配置文件源
  const srcBadge = paused
    ? pill('已暂停（维护模式）', 'warn')
    : (fileSources.length ? pill(`运行中 · ${fileSources.length} 个文件源`, 'ok') : pill('未配置文件源', ''));

  root.innerHTML = [
    pageHead(t('nav.receive'), '本应用作为日志服务器，接收交换机 / 防火墙 / 其他主机的 syslog',
      '<span class="btn" id="rx-help">帮助</span>' +
      (offline ? demoPill() : (paused ? '<span class="pill warn">采集已暂停</span>' : '<span class="pill ok">实时</span>'))),

    notice('接收端口写在 log-server-for-tos.env（默认标准端口 514/6514，需 CAP_NET_BIND_SERVICE）。若端口被占用，可改为高端口并同步调整设备端配置。'),

    /* ── 采集管理（FR-102 / FR-1107）────────────────────────── */
    card({
      title: '采集管理',
      span: 'span-all',
      badge: srcBadge,
      body: `
        <div style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-bottom:10px">
          <span class="btn ${paused ? 'primary' : ''}" id="ing-toggle">${paused ? '恢复采集' : '暂停采集（维护模式）'}</span>
          <span class="muted" style="font-size:11px">
            暂停期间：<b>停止读取</b>文件与系统日志；<b>接收端继续收并写入存储</b>（不丢数据），恢复后从上次位点续采。
          </span>
        </div>
        ${paused ? notice('采集已暂停（维护模式）。此期间不会从文件/journald 读取新日志；外部推送来的 syslog 仍会正常入库。', 'warn', 'warn') : ''}
        <div class="muted" style="font-size:12px;margin:6px 0">文件采集源（支持任意绝对路径；多行合并用于 Java 堆栈等）</div>
        <div id="src-rows" style="display:flex;flex-direction:column;gap:8px">${rowsHTML(statusByPath)}</div>
        <div style="margin-top:10px;display:flex;gap:8px;align-items:center;flex-wrap:wrap">
          <span class="btn" id="src-add">添加采集源</span>
          <span class="btn primary" id="src-save">保存采集源</span>
          <span class="muted" style="font-size:11px" id="src-msg"></span>
        </div>`,
    }),

    '<div class="grid wide" style="margin-top:12px">',

    card({
      title: '监听器',
      badge: pill(`${listeners.length} 个`),
      body: listeners.length ? `<div class="list">${listeners.map(l => listItem({
        level: l.enabled ? 'ok' : 'off',
        name: `${String(l.protocol).toUpperCase()} :${l.port}`,
        meta: l.protocol === 'udp' ? `已收报文 ${l.packets || 0}`
          : l.protocol === 'tcp' ? `累计连接 ${l.conns || 0}` : '未启用',
      })).join('')}</div>` : `<div class="empty">后端未连接</div>`,
    }),

    card({
      title: '接收统计',
      body: `${kpis([
        { v: rx.udp_packets || 0, k: 'UDP 报文' },
        { v: rx.tcp_conns || 0, k: 'TCP 连接' },
        { v: rx.dropped || 0, k: '丢弃' },
        { v: rx.queue_dropped || 0, k: '队列丢弃' },
      ])}`,
    }),

    card({
      title: '接收准入与限速',
      span: 'span-all',
      badge: (pol.allow && pol.allow.length) ? pill('已限制来源', 'ok') : pill('接受所有来源', 'warn'),
      body: `
        <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px 14px">
          <label style="font-size:var(--font-table)">允许来源 CIDR（每行一个；空 = 全部）
            <textarea id="pol-allow" class="field mono" rows="4" style="width:100%">${esc((pol.allow || []).join('\n'))}</textarea>
          </label>
          <label style="font-size:var(--font-table)">拒绝来源 CIDR（优先于允许）
            <textarea id="pol-deny" class="field mono" rows="4" style="width:100%">${esc((pol.deny || []).join('\n'))}</textarea>
          </label>
          <label style="font-size:var(--font-table)">每来源 EPS 上限（0 = 不限）
            <input id="pol-eps" class="field" type="number" min="0" value="${Number(pol.max_eps || 0)}">
          </label>
          <label style="font-size:var(--font-table)">每来源字节/秒上限（0 = 不限）
            <input id="pol-bps" class="field" type="number" min="0" value="${Number(pol.max_bytes_per_sec || 0)}">
          </label>
        </div>
        <div style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-top:10px">
          <span class="btn primary" id="pol-save">保存准入策略</span>
          <span class="mono" id="pol-msg" style="font-size:12px"></span>
        </div>
        <div class="muted" style="font-size:11px;margin-top:8px">
          支持 CIDR 或单个 IP；<b>拒绝优先</b>；loopback 默认允许（显式 deny 127.0.0.0/8 可关闭）。
          保存后<b>立即生效</b>，只影响之后收到的消息。当前已拒绝 <b>${Number(rx.acl_denied || 0)}</b> 条、限速 <b>${Number(rx.rate_limited || 0)}</b> 条。
          建议至少把 allow 设为你所在局域网网段；空 allow 会接受任何能访问 514/6514 的来源。
        </div>`,
    }),

    card({
      title: '系统日志（journald）',
      span: 'span-all',
      badge: jrn
        ? (jrn.paused ? pill('暂停中', 'warn')
          : (jrn.degraded ? pill('降级', 'warn') : (jrn.authorized ? pill('采集中', 'ok') : pill('未采集'))))
        : pill('未启用'),
      body: jrn
        ? (jrn.paused
            ? kv([['状态', '暂停中（维护模式）——恢复采集后自动继续'], ['已读条目', String(jrn.entries || 0)]])
            : (jrn.authorized
                ? kv([
                    ['状态', jrn.degraded ? `采集中（降级：${esc(jrn.degraded_reason || '未收到条目')}）` : '采集中（只读）'],
                    ['已读条目', String(jrn.entries || 0)],
                    ['其中系统条目', String(jrn.system_entries || 0)],
                    ['读取重启次数', String(jrn.restarts || 0)],
                    ['最近一条', jrn.silent_for_sec >= 0 ? `${esc(jrn.silent_for_sec)}s 前` : '—'],
                    ['journalctl 参数', `<span class="mono">${esc(jrn.args || '')}</span>`],
                  ])
                : kv([
                    ['状态', '未采集（当前无读取权限；具备权限后会自动开始，无需重装）'],
                    ['已读条目', '0'],
                    ['说明', '本应用只读且**不自我授权**：不会加系统用户组、不会改 ACL、不会改 rsyslog/journald 配置'],
                  ])))
        : `<div class="empty">未启用（可由 LOGSERVER_JOURNALD_ENABLED 开启；默认 auto = 有权限才采集）</div>`,
    }),

    card({
      title: '协议矩阵（接收侧全兼容，D-004）',
      span: 'span-all',
      badge: pill('无需用户选择', 'accent'),
      body: table(['协议', '端口', '状态', '可靠性', '典型来源'], MATRIX.map(r => r.map(esc))),
    }),

    '</div>',
  ].join('');

  bindHelp(root, helpHTML);
  bindIngest(root, paused);
  bindReceivePolicy(root);
  bindSources(root);
}

/* ── 采集源可编辑行 ───────────────────────────────────────────── */
function rowsHTML(statusByPath) {
  if (!editRows.length) return `<div class="empty">尚未配置任何文件采集源（点「添加采集源」）</div>`;
  return editRows.map((r, i) => sourceRow(r, i, statusByPath)).join('');
}

function sourceRow(r, i, statusByPath) {
  const st = statusByPath && statusByPath[r.path];
  const state = st
    ? (st.error ? pill('错误', 'crit') : (st.silent_for_sec > 600 ? pill('静默', 'warn') : pill('正常', 'ok')))
    : pill(r.enabled ? '未运行' : '已停用', '');
  const meta = st
    ? `位点 ${esc(st.offset)} · 轮转 ${esc(st.rotations)} · 已采集 ${esc(st.records)}`
    : '（保存后开始采集）';
  return `<div class="list-item" style="align-items:flex-start">
    <div style="flex:1;min-width:0;display:flex;flex-direction:column;gap:6px">
      <input class="field mono" data-path="${i}" style="width:100%" placeholder="/var/log/xxx.log" value="${esc(r.path)}">
      <div style="display:flex;gap:14px;align-items:center;font-size:12px;flex-wrap:wrap">
        <label style="display:flex;gap:5px;align-items:center"><input type="checkbox" data-en="${i}" ${r.enabled ? 'checked' : ''}> 启用</label>
        <label style="display:flex;gap:5px;align-items:center"><input type="checkbox" data-ml="${i}" ${r.multiline ? 'checked' : ''}> 多行合并</label>
        <input class="field" data-enc="${i}" style="width:150px" placeholder="编码（留空=自动）" value="${esc(r.encoding || '')}">
        <span class="muted">${meta}</span>
      </div>
    </div>
    <div style="display:flex;flex-direction:column;gap:6px;align-items:flex-end">
      ${state}
      <span class="btn" data-del="${i}" style="padding:1px 8px">删除</span>
    </div>
  </div>`;
}

function bindSources(root) {
  const box = root.querySelector('#src-rows');
  if (!box) return;
  const collect = () => {
    box.querySelectorAll('[data-path]').forEach((el) => {
      const i = Number(el.dataset.path);
      if (!editRows[i]) return;
      editRows[i].path = el.value.trim();
      const en = box.querySelector(`[data-en="${i}"]`);
      const ml = box.querySelector(`[data-ml="${i}"]`);
      const enc = box.querySelector(`[data-enc="${i}"]`);
      editRows[i].enabled = !!(en && en.checked);
      editRows[i].multiline = !!(ml && ml.checked);
      editRows[i].encoding = enc ? enc.value.trim() : '';
    });
  };
  const re = (statusByPath) => { box.innerHTML = rowsHTML(statusByPath || {}); };

  const add = root.querySelector('#src-add');
  if (add) add.addEventListener('click', () => {
    collect();
    editRows.push({ path: '', enabled: true, multiline: false, encoding: '' });
    re();
  });

  box.addEventListener('click', (e) => {
    const b = e.target.closest('[data-del]');
    if (!b) return;
    collect();
    editRows.splice(Number(b.dataset.del), 1);
    re();
  });

  const save = root.querySelector('#src-save');
  if (save) save.addEventListener('click', async () => {
    collect();
    const msg = root.querySelector('#src-msg');
    const clean = editRows.filter(r => r.path);
    if (clean.length !== editRows.length) {
      if (!window.confirm('有采集源未填路径，将被忽略。继续保存？')) return;
    }
    const r = await api.putSources(clean.map(x => ({
      path: x.path, enabled: !!x.enabled, multiline: !!x.multiline, encoding: x.encoding || '',
    })));
    if (!r.ok) { if (msg) msg.textContent = '❌ ' + ((r.error && r.error.message) || '保存失败'); return; }
    editRows = clean;
    if (msg) msg.textContent = '✅ 已保存并生效';
    render(root);
  });
}

/* ── 接收准入与限速（FR-603/604）──────────────────────────── */
function bindReceivePolicy(root) {
  const btn = root.querySelector('#pol-save');
  if (!btn) return;
  const msg = (text, kind) => {
    const el = root.querySelector('#pol-msg');
    if (el) { el.textContent = text; el.style.color = kind === 'err' ? '#c0392b' : '#2e7d32'; }
  };
  const lines = (id) => (root.querySelector(id).value || '').split('\n').map(x => x.trim()).filter(Boolean);
  btn.addEventListener('click', async () => {
    const body = {
      allow: lines('#pol-allow'),
      deny: lines('#pol-deny'),
      max_eps: Number(root.querySelector('#pol-eps').value || 0),
      max_bytes_per_sec: Number(root.querySelector('#pol-bps').value || 0),
    };
    const r = await api.putReceivePolicy(body);
    if (!r.ok) { msg('❌ ' + ((r.error && r.error.message) || '保存失败'), 'err'); return; }
    msg('✅ 已保存并立即生效');
  });
}

/* ── 采集总开关（FR-1107）──────────────────────────────────── */
function bindIngest(root, paused) {
  const tgl = root.querySelector('#ing-toggle');
  if (!tgl) return;
  tgl.addEventListener('click', async () => {
    if (!paused && !window.confirm('暂停采集（维护模式）？\n\n将停止读取文件与系统日志；接收端仍继续收并写入存储。')) return;
    const r = paused ? await api.resumeIngest() : await api.pauseIngest();
    if (!r.ok) { window.alert('操作失败：' + ((r.error && r.error.message) || '')); return; }
    render(root);
  });
}

/* ── 「帮助」弹窗 ───────────────────────────────────────────── */
function bindHelp(root, helpHTML) {
  const helpBtn = root.querySelector('#rx-help');
  if (!helpBtn) return;
  helpBtn.addEventListener('click', () => {
    openModal(root, {
      title: '发送方怎么配（地址与端口）',
      body: helpHTML,
      onMount: (modal) => bindCopy(modal.querySelectorAll('[data-copy]')),
    });
  });
}

function bindCopy(nodes) {
  nodes.forEach((b) => {
    b.addEventListener('click', async () => {
      const txt = b.getAttribute('data-copy');
      try {
        await navigator.clipboard.writeText(txt);
        b.textContent = '已复制';
        setTimeout(() => { b.textContent = '复制'; }, 1500);
      } catch {
        window.prompt('手动复制：', txt);
      }
    });
  });
}

function snippet(label, code) {
  return `<div style="margin-bottom:8px">
    <div class="muted" style="font-size:11px;margin-bottom:3px">${esc(label)}</div>
    <div style="display:flex;gap:8px;align-items:stretch">
      <code class="mono" style="flex:1;min-width:0;overflow-x:auto;white-space:pre;background:var(--surface2);border:1px solid var(--border);border-radius:6px;padding:8px 10px">${esc(code)}</code>
      <span class="btn" data-copy="${esc(code)}" style="align-self:center">复制</span>
    </div>
  </div>`;
}

function helpBody(addrs, primary, udp, tcp, tls) {
  return `
    <div class="muted" style="font-size:12px;margin-bottom:8px">把日志发往这台 NAS 时，在发送端填写下面这个<b>地址</b>与<b>端口</b>：</div>
    <dl class="kv" style="margin-bottom:12px">
      <dt>地址（任一即可）</dt><dd>${addrs.length ? addrs.map(([v, src]) => `<span class="mono">${esc(v)}</span><span class="muted" style="font-size:11px">（${esc(src)}）</span>`).join('&nbsp;&nbsp;') : '—'}</dd>
      <dt>UDP 端口</dt><dd>${udp.port ? `<span class="mono">${esc(udp.port)}</span> ${udp.enabled ? pill('监听中', 'ok') : pill('未监听', 'warn')}` : '—'}</dd>
      <dt>TCP 端口</dt><dd>${tcp.port ? `<span class="mono">${esc(tcp.port)}</span> ${tcp.enabled ? pill('监听中', 'ok') : pill('未监听', 'warn')}` : '—'}</dd>
      <dt>TLS 端口</dt><dd>${tls.port ? `<span class="mono">${esc(tls.port)}</span> ${tls.enabled ? pill('监听中', 'ok') : pill('未启用：需配置证书', 'warn')}` : '—'}</dd>
    </dl>
    <div class="muted" style="font-size:12px;margin:0 0 6px"><b>发送端配置示例</b>（已代入首选地址 <span class="mono">${esc(primary)}</span>）</div>
    ${snippet('Linux rsyslog —— UDP（快、可能丢）', `*.* @${primary}:${udp.port || 514}`)}
    ${snippet('Linux rsyslog —— TCP（推荐；断连期间本机磁盘队列补齐）', `*.* @@${primary}:${tcp.port || 514}`)}
    ${snippet('syslog-ng', `destination d_nas { syslog("${primary}" transport("tcp") port(${tcp.port || 514})); };`)}
    ${snippet('交换机 / 防火墙（通用）', `Syslog Server: ${primary}   Port: ${udp.port || 514}   Protocol: UDP`)}
    ${snippet('Docker 容器日志', `docker run --log-driver=syslog --log-opt syslog-address=udp://${primary}:${udp.port || 514} <image>`)}
    ${snippet('本机 / 其它 Linux 的一行测试', `logger -n ${primary} -P ${udp.port || 514} -d "hello from $(hostname)"`)}
    <div class="muted" style="font-size:11px;margin-top:8px">
      提示：发送端与本机需<b>网络可达</b>（同一局域网、无防火墙拦截）。不确定是否收到时，可发一条测试，再回「检索」页搜 <span class="mono">hello from</span>。
    </div>`;
}
