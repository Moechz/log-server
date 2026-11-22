/* icons.js —— 内联单色 SVG 图标（不引入任何图标库；stroke 用 currentColor）
 * 约定：统一 24×24 viewBox、stroke-width 1.6、round linecap —— 与顶栏/侧栏字重协调。 */

const P = {
  overview: '<rect x="3" y="3" width="7.5" height="7.5" rx="1.5"/><rect x="13.5" y="3" width="7.5" height="7.5" rx="1.5"/><rect x="3" y="13.5" width="7.5" height="7.5" rx="1.5"/><rect x="13.5" y="13.5" width="7.5" height="7.5" rx="1.5"/>',
  monitor:  '<path d="M3 12h3.5l2-5.5 3 11 2.5-8 1.8 2.5H21"/>',
  audit:    '<path d="M12 3 4.5 6v5.6c0 4.3 3.1 8.1 7.5 9.4 4.4-1.3 7.5-5.1 7.5-9.4V6L12 3Z"/><path d="m8.8 12 2.2 2.2 4.2-4.4"/>',
  search:   '<circle cx="10.5" cy="10.5" r="6"/><path d="m15.2 15.2 4.3 4.3"/>',
  receive:  '<path d="M12 3v10"/><path d="m8 9.5 4 4 4-4"/><path d="M4 15v3.5A1.5 1.5 0 0 0 5.5 20h13a1.5 1.5 0 0 0 1.5-1.5V15"/>',
  forward:  '<path d="M4 12h7"/><path d="m9 8 4 4-4 4"/><path d="M14 20V7.5A2.5 2.5 0 0 1 16.5 5H20"/>',
  storage:  '<ellipse cx="12" cy="6.2" rx="7" ry="2.7"/><path d="M5 6.2v11.6c0 1.5 3.1 2.7 7 2.7s7-1.2 7-2.7V6.2"/><path d="M5 12c0 1.5 3.1 2.7 7 2.7s7-1.2 7-2.7"/>',
  rules:    '<path d="M4 7h10M18 7h2M4 17h4M12 17h8"/><circle cx="16" cy="7" r="2"/><circle cx="10" cy="17" r="2"/>',
  about:    '<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5.5"/><circle cx="12" cy="8" r=".9" fill="currentColor" stroke="none"/>',
  settings: '<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>',
  warn:     '<path d="M12 4.5 3.5 19h17L12 4.5Z"/><path d="M12 10v4"/><circle cx="12" cy="16.6" r=".9" fill="currentColor" stroke="none"/>',
  info:     '<circle cx="12" cy="12" r="8.5"/><path d="M12 12.5v4"/><circle cx="12" cy="9" r=".9" fill="currentColor" stroke="none"/>',
  clock:    '<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>',
  check:    '<path d="m5 12.5 4.5 4.5L19 7.5"/>',
  plus:     '<path d="M12 5v14M5 12h14"/>',
};

/** 生成一个 SVG 字符串（用于 innerHTML 拼接） */
export function icon(name, size = 16, cls = '') {
  const body = P[name] || P.info;
  return `<svg viewBox="0 0 24 24" width="${size}" height="${size}" fill="none"
    stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"
    ${cls ? `class="${cls}"` : ''} aria-hidden="true">${body}</svg>`;
}

export const iconNames = Object.keys(P);
