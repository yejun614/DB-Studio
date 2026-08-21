// 감사 로그 조회 화면 (슈퍼 어드민 전용).
import { api } from '../core/api.js';
import {
  h, mount, icon, input, select, spinner, emptyState, pageHeader,
  badge, formatDate, toastError, openModal, copyToClipboard,
} from '../core/ui.js';
import { errorPanel } from './users.js';

const PAGE_SIZE = 50;

// 액션 접두사로 그룹 필터를 제공한다. 서버는 LIKE 'prefix%'로 매칭한다.
const ACTION_GROUPS = [
  { value: '', label: '전체' },
  { value: 'auth.', label: '인증' },
  { value: 'user.', label: '사용자' },
  { value: 'connection.', label: '커넥션' },
  { value: 'access.', label: '권한' },
  { value: 'system.', label: '시스템' },
];

export async function renderAudit(outlet, _params, query) {
  const filter = {
    action: query.get('action') ?? '',
    offset: Number(query.get('offset') ?? 0),
  };

  const actionSelect = select(ACTION_GROUPS, { value: filter.action });
  const searchInput = input({ placeholder: '대상 ID로 검색', value: query.get('targetId') ?? '' });

  const body = h('div');
  mount(outlet,
    pageHeader('감사 로그', '계정·권한·커넥션 변경 이력'),
    h('div.card.filter-bar', {},
      h('label.field.field-inline', {}, h('span.field-label', {}, '분류'), actionSelect),
      h('label.field.field-inline', {}, h('span.field-label', {}, '대상'), searchInput),
      h('button.btn', { type: 'button', onclick: () => load(0) }, icon('refresh'), '조회'),
    ),
    body,
  );

  async function load(offset) {
    mount(body, spinner());
    const params = new URLSearchParams();
    if (actionSelect.value) params.set('action', actionSelect.value);
    if (searchInput.value.trim()) params.set('targetId', searchInput.value.trim());
    params.set('limit', String(PAGE_SIZE));
    params.set('offset', String(offset));

    try {
      const res = await api.get(`/audit?${params}`);
      if (res.entries.length === 0) {
        mount(body, emptyState('조건에 맞는 로그가 없습니다'));
        return;
      }
      mount(body,
        h('div.card', {}, table(res.entries)),
        pager(res.total, offset, load),
      );
    } catch (err) {
      mount(body, errorPanel(err));
      toastError(err);
    }
  }

  await load(filter.offset);
}

function table(entries) {
  return h('table.table.audit-table', {},
    h('thead', {}, h('tr', {},
      h('th', {}, '시각'),
      h('th', {}, '수행자'),
      h('th', {}, '액션'),
      h('th', {}, '대상'),
      h('th', {}, '결과'),
      h('th', {}, '상세'),
    )),
    // 줄을 누르면 상세가 열린다. 표의 마지막 칸에 밀어 넣은 상세는 길면 잘리고,
    // 잘린 자리에 무엇이 있었는지는 화면에서 알 수 없다.
    h('tbody', {}, entries.map((e) => h('tr.is-clickable', {
      tabindex: 0,
      title: '자세히 보기',
      onclick: () => openEntry(e),
      onkeydown: (ev) => {
        if (ev.key === 'Enter' || ev.key === ' ') {
          ev.preventDefault();
          openEntry(e);
        }
      },
    },
      h('td.nowrap', {}, formatDate(e.at)),
      h('td', {}, e.actorName || '—'),
      h('td', {}, h('code.action-code', {}, e.action)),
      h('td', {}, e.targetType ? `${e.targetType}${e.targetId ? ` · ${short(e.targetId)}` : ''}` : '—'),
      h('td', {}, resultBadge(e.result)),
      h('td.detail-cell', {}, detailText(e.detail)),
    ))),
  );
}

// openEntry는 기록 하나를 펼쳐 보여준다.
//
// 상세(detail)를 표로 그리는 이유: 지금까지는 `key=value, key=value…` 한 줄이라
// 값에 쉼표가 들어가면 어디까지가 한 값인지 읽을 수 없었다. 키와 값을 칸으로
// 나누면 그 모호함이 사라진다.
function openEntry(e) {
  const rows = Object.entries(e.detail ?? {});
  const text = () => JSON.stringify({
    at: e.at, actor: e.actorName, actorId: e.actorId,
    action: e.action, target: e.targetType, targetId: e.targetId,
    result: e.result, ip: e.ip, detail: e.detail,
  }, null, 2);

  openModal({
    title: '감사 기록',
    width: 680,
    body: () => [
      h('div.audit-detail-head', {},
        h('div.kv', {}, h('span.kv-key', {}, '시각'), h('span', {}, formatDate(e.at))),
        h('div.kv', {}, h('span.kv-key', {}, '수행자'),
          h('span', {}, e.actorName || '—')),
        h('div.kv', {}, h('span.kv-key', {}, '액션'),
          h('code.action-code', {}, e.action)),
        h('div.kv', {}, h('span.kv-key', {}, '대상'),
          h('span', {}, e.targetType
            ? `${e.targetType}${e.targetId ? ` · ${e.targetId}` : ''}` : '—')),
        h('div.kv', {}, h('span.kv-key', {}, '결과'), resultBadge(e.result)),
        e.ip ? h('div.kv', {}, h('span.kv-key', {}, 'IP'), h('code', {}, e.ip)) : null,
      ),
      h('h3.audit-detail-title', {}, '상세'),
      rows.length === 0
        ? h('p.muted', {}, '추가 정보가 없습니다')
        : h('div.table-wrap', {}, h('table.table.audit-kv', {},
          h('thead', {}, h('tr', {}, h('th', {}, '항목'), h('th', {}, '값'))),
          h('tbody', {}, rows.map(([k, v]) => h('tr', {},
            h('td.nowrap', {}, h('code', {}, k)),
            h('td', {}, h('pre.audit-kv-value', {},
              typeof v === 'object' && v !== null ? JSON.stringify(v, null, 2) : String(v))),
          ))),
        )),
    ],
    footer: (close) => [
      h('button.btn', {
        type: 'button',
        onclick: () => copyToClipboard(text()),
      }, icon('copy'), '전체 복사'),
      h('button.btn', { type: 'button', onclick: close }, '닫기'),
    ],
  });
}

function resultBadge(result) {
  if (result === 'ok') return badge('성공', 'success');
  if (result === 'denied') return badge('거부', 'warn');
  return badge('실패', 'danger');
}

function detailText(detail) {
  if (!detail || Object.keys(detail).length === 0) return '—';
  return Object.entries(detail)
    .map(([k, v]) => `${k}=${typeof v === 'object' ? JSON.stringify(v) : v}`)
    .join(', ');
}

function short(id) {
  return id.length > 12 ? `${id.slice(0, 8)}…` : id;
}

function pager(total, offset, load) {
  const from = offset + 1;
  const to = Math.min(offset + PAGE_SIZE, total);
  return h('div.pager', {},
    h('span.muted', {}, `${from}–${to} / ${total}`),
    h('button.btn.btn-small', {
      type: 'button', disabled: offset === 0,
      onclick: () => load(Math.max(0, offset - PAGE_SIZE)),
    }, '이전'),
    h('button.btn.btn-small', {
      type: 'button', disabled: to >= total,
      onclick: () => load(offset + PAGE_SIZE),
    }, '다음'),
  );
}
