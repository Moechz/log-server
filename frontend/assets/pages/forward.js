/* pages/forward.js —— 转发（真实目标配置：GET/PUT /api/v1/forward/targets） */
import { card, kpis, progress, listItem, table, notice, demoPill, pill, pageHead, esc, select, initSelects } from '../ui.js?v=__ASSET_VER__';
import { t } from '../i18n.js?v=__ASSET_VER__';
import * as api from '../api.js?v=__ASSET_VER__';

const PRESETS = [
  ['发给标准 syslog 服务器', 'TCP + RFC 5424（推荐默认）'],
  ['发给不支持 TLS 的老设备 / 交换机', 'UDP 514 + RFC 3164'],
  ['跨公网传输', 'TLS 6514（可配自签 CA / 指纹固定）'],
  ['大规模汇聚', 'HTTP NDJSON 批量（Loki/ES/OpenObserve 兼容）'],
];

export async function render(root) {
  const r = await api.forwardTargets(null);
  const offline = r.offline || !r.ok;
  const targets = (!offline && r.data.targets) || [];

  root.innerHTML = [
    pageHead(t('nav.forward'), '把日志发给另一台日志服务器：协议可选、断网不丢、原始内容无损',
      offline ? demoPill() : '<span class="pill ok">实时</span>'),

    notice('默认「TCP + RFC 5424 + 磁盘队列」（D-004）：TCP syslog 断连期间会丢日志，必须靠磁盘队列补齐。转发默认透明 —— 允许重新封装头部，但 payload 逐字节无损（D-005）。'),

    '<div class="grid wide" style="margin-top:12px">',

    card({
      title: '转发目标',
      badge: pill(`${targets.length} 个`),
      span: 'span-all',
      body: targets.length ? `<div class="list">${targets.map(f => listItem({
        level: f.healthy ? 'ok' : (f.enabled ? 'warn' : 'off'),
        name: `${f.name} — ${f.protocol}://${f.addr}`,
        meta: `已发 ${f.sent} · 失败 ${f.failed} · 积压 ${f.queue.depth}（${f.queue.bytes} B）` +
              (f.queue.retried ? ` · 重试 ${f.queue.retried}` : '') +
              (f.last_ok ? ` · 最后成功 ${f.last_ok.slice(11, 19)}` : '') +
              (f.last_err ? ` · 最近错误：${f.last_err}` : '') +
              (f.filter_empty ? ' · 过滤：全转发' : ' · 过滤：已设置'),
        right: `<span class="pill ${f.enabled ? 'ok' : ''}">${f.enabled ? '启用' : '停用'}</span>
                <button class="btn" data-test="${esc(f.id)}">测试</button>`,
      })).join('')}</div>` : `<div class="empty">尚未配置转发目标</div>`,
    }),

    card({
      title: '队列与投递',
      span: 'span-all',
      body: targets.length ? `<div class="list">${targets.map(f => `
        <div class="list-item" style="flex-direction:column;align-items:stretch;gap:6px">
          <div style="display:flex;gap:8px;align-items:center">
            <span class="name">${esc(f.name)}</span>
            <span class="muted">积压 ${f.queue.depth} 条 / ${f.queue.bytes} B · 段 ${f.queue.segments} 个</span>
            <span style="margin-left:auto">${pill(f.queue.depth > 1000 ? '积压偏高' : '正常', f.queue.depth > 1000 ? 'warn' : 'ok')}</span>
          </div>
          ${progress(Math.min(100, f.queue.depth / 100), f.queue.depth > 1000 ? 'warn' : '')}
        </div>`).join('')}</div>` : `<div class="empty">—</div>`,
    }),

    card({
      title: '协议选择（场景预设优先）',
      span: 'span-all',
      body: table(['场景', '推荐配置'], PRESETS.map(x => x.map(esc))) +
        `<div class="muted" style="font-size:11px;margin-top:8px">
          UDP 目标无法探测可达性（无连接语义）；TCP/HTTP 可点「测试」做连通性检查。
          当前支持 udp / tcp / tls / http 四种协议。
        </div>`,
    }),

    card({
      title: '添加目标（写入 config.json）',
      span: 'span-all',
      body: `
        <div style="display:grid;grid-template-columns:auto 1fr auto 1fr;gap:8px 10px;align-items:center;font-size:13px">
          <label>名称</label><input id="fw-name" class="field" placeholder="SIEM">
          <label>协议</label>
          ${select('fw-proto', 'tcp', [['tcp', 'tcp'], ['udp', 'udp'], ['tls', 'tls'], ['http', 'http']])}
          <label>地址</label><input id="fw-addr" class="field" placeholder="siem.corp.local:514">
          <label>仅审计类</label>
          ${select('fw-audit', 'false', [['false', '否（全部）'], ['true', '是']])}
        </div>
        <div style="margin-top:12px;display:flex;gap:8px">
          <button class="btn primary" id="fw-add">添加并保存</button>
          <button class="btn" id="fw-reload">刷新</button>
        </div>
        <div id="fw-msg" style="margin-top:10px"></div>`,
    }),

    '</div>',
  ].join('');

  // 连通性测试
  root.querySelectorAll('[data-test]').forEach(btn => {
    btn.addEventListener('click', async () => {
      const id = btn.getAttribute('data-test');
      btn.disabled = true;
      const tr = await api.testForwardTarget(id);
      btn.disabled = false;
      const msg = tr.ok ? `✅ ${tr.data.message}` : `❌ ${tr.error ? tr.error.message : '测试失败'}`;
      const bar = document.getElementById('fw-msg');
      if (bar) bar.innerHTML = `<div class="notice">${esc(msg)}</div>`;
    });
  });

  const add = root.querySelector('#fw-add');
  if (add) {
    add.addEventListener('click', async () => {
      const name = root.querySelector('#fw-name').value.trim();
      const proto = root.querySelector('#fw-proto').value;
      const addr = root.querySelector('#fw-addr').value.trim();
      const onlyAudit = root.querySelector('#fw-audit').value === 'true';
      const msg = root.querySelector('#fw-msg');
      if (!addr) { msg.innerHTML = `<div class="notice crit">请填写地址</div>`; return; }

      const cur = targets.filter(x => x.protocol && x.addr).map(x => ({
        id: x.id, name: x.name, protocol: x.protocol, addr: x.addr, enabled: x.enabled,
      }));
      cur.push({
        id: 't' + (cur.length + 1) + '-' + Date.now().toString(36),
        name: name || addr, protocol: proto, addr, enabled: true,
        filter: onlyAudit ? { only_audit: true } : {},
      });
      const res = await api.putForwardTargets(cur);
      msg.innerHTML = res.ok
        ? `<div class="notice">已保存，目标列表已更新</div>`
        : `<div class="notice crit">保存失败：${esc(res.error ? res.error.message : '')}</div>`;
      if (!res.ok) return;
      const keep = root.innerHTML;
      await render(root);
      initSelects(root);   // 自重新渲染后重新初始化下拉（.sel）
      const bar = root.querySelector('#fw-msg');
      if (bar) bar.innerHTML = `<div class="notice">已保存 ${esc(addr)}</div>`;
      void keep;
    });
  }
  const reload = root.querySelector('#fw-reload');
  if (reload) reload.addEventListener('click', () => render(root));
}
