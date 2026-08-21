// 오른쪽 인스펙터 폭 조절.
//
// ERD 설계와 구조 화면이 같은 골격(.erd-body)을 쓰므로 손잡이도 한 벌만 둔다.
// 두 벌이면 한쪽만 고쳐지고, 그 차이는 "이 화면에서는 왜 안 되지"로 나타난다.
//
// 폭은 이 브라우저에 기억한다. 넓은 모니터와 노트북에서 원하는 폭이 다르고,
// 그것은 서버가 알아야 할 설정이 아니다.
import { h } from './dom.js';

const MIN = 260;
const MAX = 760;
const DEFAULT = 340;

export function panelResizeHandle() {
  return h('div.erd-panel-resize', {
    role: 'separator',
    'aria-orientation': 'vertical',
    'aria-label': '속성 창 폭 조절',
    'aria-valuemin': String(MIN),
    'aria-valuemax': String(MAX),
    tabindex: '0',
    title: '드래그해서 폭 조절 (두 번 누르면 기본값)',
  });
}

/**
 * attachPanelResize는 손잡이에 동작을 붙인다.
 * @param {object} opts
 * @param {HTMLElement} opts.root    폭 변수를 받을 요소(.erd-editor)
 * @param {HTMLElement} opts.handle  panelResizeHandle()이 만든 손잡이
 * @param {string} opts.storageKey   기억할 자리
 * @param {() => void} [opts.onResize] 폭이 바뀐 뒤 할 일(캔버스 다시 그리기 등)
 */
export function attachPanelResize({ root, handle, storageKey, onResize }) {
  const clamp = (v) => Math.max(MIN, Math.min(MAX, Math.round(v)));

  const apply = (width, persist = true) => {
    const w = clamp(width);
    root.style.setProperty('--erd-panel-width', `${w}px`);
    handle.setAttribute('aria-valuenow', String(w));
    if (persist) {
      try {
        localStorage.setItem(storageKey, String(w));
      } catch { /* 저장소를 못 써도 이번 세션은 그대로 쓴다 */ }
    }
    onResize?.();
  };

  let saved = DEFAULT;
  try {
    saved = Number(localStorage.getItem(storageKey)) || DEFAULT;
  } catch { /* 기본값을 쓴다 */ }
  apply(saved, false);

  handle.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    handle.setPointerCapture(e.pointerId);
    // 오른쪽 끝을 기준으로 잰다. 손잡이의 위치가 아니라 화면 끝과의 거리가
    // 곧 패널 폭이므로, 끄는 동안 손잡이가 움직여도 계산이 어긋나지 않는다.
    const right = root.getBoundingClientRect().right;
    const move = (ev) => apply(right - ev.clientX, false);
    const up = (ev) => {
      handle.releasePointerCapture(ev.pointerId);
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
      apply(right - ev.clientX); // 놓을 때만 저장한다.
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', up);
  });

  handle.addEventListener('dblclick', () => apply(DEFAULT));

  // 마우스가 없는 사람도 조절할 수 있어야 한다.
  handle.addEventListener('keydown', (e) => {
    const now = Number(handle.getAttribute('aria-valuenow')) || DEFAULT;
    const step = e.shiftKey ? 40 : 10;
    if (e.key === 'ArrowLeft') apply(now + step);
    else if (e.key === 'ArrowRight') apply(now - step);
    else if (e.key === 'Home') apply(MIN);
    else if (e.key === 'End') apply(MAX);
    else return;
    e.preventDefault();
  });
}
