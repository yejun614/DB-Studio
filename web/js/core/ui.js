// 공용 UI 조각: 토스트, 모달, 폼 필드, 배지, 빈 상태.
import { h, mount, clear, icon } from './dom.js';

// ---------- 토스트 ----------

export function toast(message, kind = 'info', timeout = 4000) {
  const box = document.getElementById('toasts');
  if (!box) return;
  const el = h(`div.toast.toast-${kind}`, {},
    icon(kind === 'error' ? 'alert' : kind === 'success' ? 'check' : 'activity'),
    h('span', {}, message),
    h('button.toast-close', { type: 'button', title: '닫기', onclick: () => el.remove() }, '×'),
  );
  box.appendChild(el);
  if (timeout > 0) setTimeout(() => el.remove(), timeout);
}

export function toastError(err) {
  const message = err?.message || '알 수 없는 오류가 발생했습니다';
  toast(err?.detail ? `${message} (${err.detail})` : message, 'error', 6000);
}

// ---------- 모달 ----------

// openModal은 모달을 띄우고 close 함수를 반환한다.
// body는 (close) => Node 형태의 함수 또는 Node.
//
// 바깥을 눌러도 닫히지 않는다. 닫는 길은 오른쪽 위 X와 Esc 두 개다.
export function openModal({ title, body, footer, width = 560, onClose }) {
  const overlay = h('div.modal-overlay');
  const close = () => {
    overlay.remove();
    document.removeEventListener('keydown', onKey);
    onClose?.();
  };
  const onKey = (e) => {
    if (e.key !== 'Escape') return;
    // 모달 위에 모달이 열릴 수 있다(예: 트리거 목록 위의 트리거 편집).
    // 이때 Esc는 맨 위 하나만 닫아야 한다 — 둘 다 닫히면 방금 연 편집창을 취소하려다
    // 목록까지 사라져 처음부터 다시 찾아 들어가야 한다.
    const stack = document.querySelectorAll('.modal-overlay');
    if (stack[stack.length - 1] !== overlay) return;
    close();
  };

  const content = typeof body === 'function' ? body(close) : body;
  const foot = typeof footer === 'function' ? footer(close) : footer;

  // tabindex 를 두는 이유: 달라고 한 칸이 없을 때 여기에 포커스를 준다.
  // 포커스가 대화상자 밖(뒤 화면)에 남아 있으면 Tab 이 뒤 화면을 돌아다닌다.
  const dialog = h('div.modal', {
    style: { maxWidth: `${width}px` }, role: 'dialog', 'aria-modal': 'true', tabindex: '-1',
  },
    h('header.modal-head', {},
      h('h2', {}, title),
      h('button.icon-btn', { type: 'button', title: '닫기', onclick: close }, icon('x')),
    ),
    h('div.modal-body', {}, content),
    foot ? h('footer.modal-foot', {}, foot) : null,
  );

  overlay.appendChild(dialog);
  // 바깥을 눌러도 닫지 않는다.
  //
  // 이 앱의 모달은 대부분 무언가를 고치는 자리다(커넥션 설정, 트리거, 컬럼 편집).
  // 긴 폼을 채우다 캔버스 여백을 한 번 잘못 누르면 입력이 통째로 사라지는데,
  // 그때 잃는 것에 비해 "바깥 클릭으로 닫기"가 주는 편의는 작다.
  // 닫는 길은 두 개 남겨 둔다: 오른쪽 위 X와 Esc.
  document.addEventListener('keydown', onKey);
  document.body.appendChild(overlay);

  // 포커스는 **달라고 한 칸에만** 준다(autofocus).
  //
  // 예전에는 첫 입력 요소를 자동으로 붙잡았는데, 그 칸이 고르개면 창을 열자마자
  // 목록이 펼쳐진다 — 읽으려고 연 창에서 목록이 화면을 덮는다. 달라고 한 칸이
  // 없으면 대화상자 자체에 포커스를 둔다: Esc·Tab 은 그대로 되고, 아무 칸도
  // 건드리지 않는다.
  const wanted = dialog.querySelector('[autofocus]');
  if (wanted) wanted.focus();
  else dialog.focus();

  return close;
}

// confirmDialog는 확인/취소 모달을 Promise로 감싼다.
// details에는 문장으로 풀면 오히려 안 읽히는 것(삭제 대상 목록 같은)을 넣는다.
// message는 <p> 안에 들어가므로 목록이나 표를 넣을 수 없다.
export function confirmDialog({
  title, message, confirmLabel = '확인', danger = false, requireText = null, details = null,
}) {
  return new Promise((resolve) => {
    let settled = false;
    let input = null;

    const submit = (close) => {
      if (requireText && input?.value !== requireText) {
        toast(`확인을 위해 "${requireText}" 를 정확히 입력하세요`, 'error');
        return;
      }
      settled = true;
      close();
      resolve(true);
    };

    const close = openModal({
      title,
      width: 480,
      body: () => {
        const parts = [h('p.modal-message', {}, message)];
        if (details) parts.push(details);
        if (requireText) {
          input = h('input.input', { type: 'text', placeholder: requireText, autocomplete: 'off' });
          parts.push(h('label.field', {},
            h('span.field-label', {}, `계속하려면 "${requireText}" 를 입력하세요`),
            input,
          ));
        }
        return parts;
      },
      footer: (closeFn) => [
        h('button.btn', { type: 'button', onclick: closeFn }, '취소'),
        h('button', {
          type: 'button',
          class: danger ? 'btn btn-danger' : 'btn btn-primary',
          onclick: () => submit(closeFn),
        }, confirmLabel),
      ],
      onClose: () => {
        if (!settled) resolve(false);
      },
    });
    void close;
  });
}

// ---------- 폼 ----------

export function field(label, control, help) {
  return h('label.field', {},
    h('span.field-label', {}, label),
    control,
    help ? h('span.field-help', {}, help) : null,
  );
}

export function input(props = {}) {
  return h('input.input', { type: 'text', ...props });
}

export function select(options, props = {}) {
  const el = h('select.input', props);
  for (const opt of options) {
    el.appendChild(h('option', { value: opt.value, selected: opt.value === props.value }, opt.label));
  }
  // props.value를 마지막에 한 번 더 지정해 selected 속성 순서 문제를 피한다.
  if (props.value !== undefined) el.value = props.value;
  return el;
}

export function textarea(props = {}) {
  return h('textarea.input.textarea', { rows: 3, ...props });
}

export function checkbox(label, props = {}) {
  const box = h('input', { type: 'checkbox', ...props });
  return h('label.checkbox', {}, box, h('span', {}, label));
}

// ---------- 표시 요소 ----------

export function badge(text, kind = 'neutral') {
  return h(`span.badge.badge-${kind}`, {}, text);
}

export function envBadge(env) {
  return badge(env === 'prod' ? '운영' : '개발', env === 'prod' ? 'danger' : 'info');
}

export function levelBadge(level) {
  const map = {
    none: ['없음', 'neutral'],
    monitor: ['모니터링', 'info'],
    erd: ['ERD 설계', 'accent'],
    migrate: ['마이그레이션', 'warn'],
  };
  const [label, kind] = map[level] ?? ['없음', 'neutral'];
  return badge(label, kind);
}

export function roleBadge(role) {
  const map = {
    superadmin: ['슈퍼 어드민', 'danger'],
    admin: ['어드민', 'warn'],
    member: ['멤버', 'neutral'],
  };
  const [label, kind] = map[role] ?? [role, 'neutral'];
  return badge(label, kind);
}

export function emptyState(message, action) {
  return h('div.empty', {}, icon('list', 28), h('p', {}, message), action ?? null);
}

export function spinner(message = '불러오는 중…') {
  return h('div.loading', {}, h('div.spinner'), h('span', {}, message));
}

export function pageHeader(title, subtitle, actions) {
  return h('header.page-head', {},
    h('div', {}, h('h1', {}, title), subtitle ? h('p.page-sub', {}, subtitle) : null),
    actions ? h('div.page-actions', {}, actions) : null,
  );
}

// ---------- 유틸 ----------

export function formatDate(value) {
  if (!value) return '—';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '—';
  const pad = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function relativeTime(value) {
  if (!value) return '—';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '—';
  const diff = Date.now() - d.getTime();
  const min = Math.floor(diff / 60000);
  if (min < 1) return '방금';
  if (min < 60) return `${min}분 전`;
  const hour = Math.floor(min / 60);
  if (hour < 24) return `${hour}시간 전`;
  const day = Math.floor(hour / 24);
  if (day < 30) return `${day}일 전`;
  return formatDate(value);
}

export async function copyToClipboard(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('클립보드에 복사했습니다', 'success', 2000);
  } catch {
    // 보안 컨텍스트가 아니면 clipboard API가 막힌다. 선택 가능한 대체 경로를 안내한다.
    toast('클립보드 복사가 차단되었습니다. 직접 선택해 복사하세요', 'error');
  }
}

export { h, mount, clear, icon };
