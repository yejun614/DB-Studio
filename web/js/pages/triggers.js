// 매크로 자동 실행 트리거 — 정기 실행(스케줄)과 조건 실행(이벤트).
//
// 두 종류를 한 화면에서 다루는 이유: 사용자가 답하려는 질문은 하나다 —
// "이 매크로가 언제 저절로 도는가". 시각이든 조건이든 그 답의 일부다.
import { api } from '../core/api.js';
import { state } from '../core/store.js';
import {
  h, mount, icon, input, select, spinner, badge, field, emptyState, pageHeader,
  toast, toastError, openModal, confirmDialog, formatDate, relativeTime,
} from '../core/ui.js';
import { errorPanel } from './users.js';
import { serverDbPicker } from '../core/connpick.js';

// 흔한 주기는 골라 쓰게 한다. cron 식을 외워야만 쓸 수 있는 기능은 절반만 있는 것이다.
const CRON_PRESETS = [
  { value: '*/10 * * * *', label: '10분마다' },
  { value: '0 * * * *', label: '매시 정각' },
  { value: '0 */6 * * *', label: '6시간마다' },
  { value: '0 3 * * *', label: '매일 새벽 3시' },
  { value: '0 9 * * 1', label: '매주 월요일 오전 9시' },
  { value: '0 4 1 * *', label: '매월 1일 새벽 4시' },
];

const EVENT_KINDS = [
  { value: '', label: '모든 이벤트' },
  { value: 'threshold', label: '임계치 위반' },
  { value: 'connectivity', label: '접속 실패' },
  { value: 'drift', label: '스키마 외부 변경' },
  { value: 'collect_error', label: '지표 수집 실패' },
];

const SEVERITIES = [
  { value: '', label: '모든 심각도' },
  { value: 'info', label: 'info 이상' },
  { value: 'warning', label: 'warning 이상' },
  { value: 'critical', label: 'critical만' },
];

// triggerPanel은 한 매크로의 트리거 목록을 그린다.
export function triggerPanel(macroID, macroParams) {
  const box = h('div');
  const panel = h('section.card', {},
    h('div.card-title', {},
      h('span', {}, '자동 실행'),
      h('button.btn.btn-small', {
        type: 'button',
        onclick: () => openTriggerDialog(macroID, macroParams, null, load),
      }, icon('plus'), '트리거 추가'),
    ),
    h('p.field-help', {},
      '트리거는 ', h('b', {}, '만든 사람의 권한으로'), ' 실행됩니다. ' +
      '계정이 비활성화되거나 권한이 회수되면 그 트리거는 실패하고 자동으로 꺼집니다.'),
    box,
  );

  async function load() {
    mount(box, spinner('트리거를 불러오는 중…'));
    let res;
    try {
      res = await api.get(`/macros/triggers?macro=${encodeURIComponent(macroID)}`);
    } catch (err) {
      mount(box, h('p.notice.notice-danger', {}, icon('alert'), err.message));
      return;
    }
    mount(box, res.items.length === 0
      ? h('p.muted.small', {}, '자동 실행이 설정되어 있지 않습니다. 이 매크로는 사람이 누를 때만 실행됩니다.')
      : h('div.trigger-list', {}, res.items.map((item) =>
          triggerRow(item, macroID, macroParams, load))));
  }

  load();
  return panel;
}

// triggerRow는 트리거 한 줄이다.
//
// 버튼은 item.canManage 로 정한다. 이 값은 서버가 매크로 접근 권한으로 계산해 준 것이다 —
// 자동 실행은 소유자의 권한으로 도는 것이라, 매크로를 볼 수 있다고 만질 수 있는 것은 아니다.
function triggerRow(item, macroID, macroParams, reload) {
  const t = item.trigger;
  const dead = !t.enabled;
  if (!item.canManage) {
    return h('div.trigger-row', { class: dead ? 'is-off' : '' },
      h('div.trigger-main', {},
        h('div.trigger-name', {},
          t.name,
          badge(t.kind === 'schedule' ? '정기' : '조건', t.kind === 'schedule' ? 'accent' : 'info'),
          dead ? badge('꺼짐', 'neutral') : null,
          t.lastStatus ? statusBadge(t.lastStatus) : null,
        ),
        h('div.trigger-when', {}, describeTrigger(t, item.describe)),
        h('div.muted.small', {}, `${t.ownerName || '알 수 없음'} 소유 · 조회만 가능`),
      ),
    );
  }

  return h('div.trigger-row', { class: dead ? 'is-off' : '' },
    h('div.trigger-main', {},
      h('div.trigger-name', {},
        t.name,
        badge(t.kind === 'schedule' ? '정기' : '조건', t.kind === 'schedule' ? 'accent' : 'info'),
        dead ? badge('꺼짐', 'neutral') : null,
        t.lastStatus ? statusBadge(t.lastStatus) : null,
      ),
      h('div.trigger-when', {}, describeTrigger(t, item.describe)),
      h('div.muted.small', {},
        `${t.ownerName || '알 수 없음'} 소유`,
        t.lastFiredAt ? ` · 마지막 발화 ${relativeTime(t.lastFiredAt)}` : ' · 발화 기록 없음',
        t.kind === 'schedule' && t.enabled && t.nextRunAt
          ? ` · 다음 ${formatDate(t.nextRunAt)}`
          : '',
      ),
      t.lastError ? h('div.cell-sub.text-danger', {}, t.lastError) : null,
    ),
    h('div.trigger-actions', {},
      h('button.btn.btn-small', {
        type: 'button',
        onclick: async () => {
          try {
            await api.post(`/macros/triggers/${t.id}/toggle`, { enabled: !t.enabled });
            toast(t.enabled ? '트리거를 껐습니다' : '트리거를 켰습니다', 'success');
            reload();
          } catch (err) { toastError(err); }
        },
      }, t.enabled ? '끄기' : '켜기'),
      h('button.btn.btn-small', {
        type: 'button',
        onclick: () => openTriggerDialog(macroID, macroParams, t, reload),
      }, icon('edit'), '수정'),
      h('button.icon-btn.danger', {
        type: 'button', title: '삭제',
        onclick: async () => {
          const ok = await confirmDialog({
            title: '트리거 삭제',
            message: `${t.name} 을(를) 삭제합니다. 이 매크로는 더 이상 자동으로 실행되지 않습니다.`,
            confirmLabel: '삭제', danger: true,
          });
          if (!ok) return;
          try {
            await api.del(`/macros/triggers/${t.id}`);
            toast('트리거를 삭제했습니다', 'success');
            reload();
          } catch (err) { toastError(err); }
        },
      }, icon('trash')),
    ),
  );
}

function statusBadge(status) {
  const map = {
    started: ['시작됨', 'info'],
    success: ['성공', 'success'],
    failed: ['실패', 'danger'],
    skipped: ['건너뜀', 'neutral'],
    disabled: ['자동 중지', 'warn'],
  };
  const [label, kind] = map[status] ?? [status, 'neutral'];
  return badge(label, kind);
}

function describeTrigger(t, describe) {
  if (t.kind === 'schedule') {
    const tz = t.timezone ? ` (${t.timezone})` : '';
    return `${describe || t.cron}${tz}`;
  }
  const parts = [];
  parts.push(EVENT_KINDS.find((k) => k.value === t.eventKind)?.label ?? '모든 이벤트');
  if (t.eventSeverity) parts.push(`${t.eventSeverity} 이상`);
  if (t.eventMetric) parts.push(`지표 ${t.eventMetric}`);
  parts.push(t.connectionId ? '지정 커넥션' : '모든 커넥션');
  parts.push(`최소 간격 ${t.minIntervalSec}초`);
  return parts.join(' · ');
}

// ---------- 만들기/수정 ----------

async function openTriggerDialog(macroID, macroParams, existing, reload) {
  let conns = { items: [] };
  try {
    conns = await api.get('/connections/');
  } catch {
    // 커넥션을 못 읽어도 스케줄 트리거는 만들 수 있다. 조건 트리거의 커넥션 선택만
    // "전체"로 제한된다.
  }

  const isEdit = Boolean(existing);
  const name = input({ value: existing?.name ?? '', placeholder: '예: 야간 정리' });
  const kind = select([
    { value: 'schedule', label: '정기 실행 — 정해진 시각에' },
    { value: 'event', label: '조건 실행 — 모니터링 이벤트가 생기면' },
  ], { value: existing?.kind ?? 'schedule', disabled: isEdit });

  // 스케줄 필드
  const presetSelect = select(
    [{ value: '', label: '직접 입력' }, ...CRON_PRESETS.map((p) => ({ value: p.value, label: p.label }))],
    { value: CRON_PRESETS.some((p) => p.value === existing?.cron) ? existing.cron : '' },
  );
  const cronInput = input({ value: existing?.cron ?? '0 3 * * *', placeholder: '분 시 일 월 요일' });
  const tzInput = input({
    value: existing?.timezone ?? guessTimezone(),
    placeholder: 'Asia/Seoul (비우면 서버 시간)',
  });
  const preview = h('div.cron-preview');

  presetSelect.addEventListener('change', () => {
    if (presetSelect.value) {
      cronInput.value = presetSelect.value;
      refreshPreview();
    }
  });
  cronInput.addEventListener('input', () => { presetSelect.value = ''; });
  cronInput.addEventListener('change', refreshPreview);
  tzInput.addEventListener('change', refreshPreview);

  async function refreshPreview() {
    mount(preview, h('span.muted.small', {}, '계산 중…'));
    try {
      const res = await api.post('/macros/triggers/preview', {
        cron: cronInput.value.trim(),
        timezone: tzInput.value.trim(),
        count: 5,
      });
      mount(preview,
        h('div.cron-describe', {}, res.describe),
        h('ul.cron-times', {}, res.next.map((t) => h('li', {}, formatDate(t)))),
      );
    } catch (err) {
      mount(preview, h('p.notice.notice-danger', {}, icon('alert'), err.message));
    }
  }

  // 이벤트 필드
  const eventKind = select(EVENT_KINDS, { value: existing?.eventKind ?? '' });
  const severity = select(SEVERITIES, { value: existing?.eventSeverity ?? 'warning' });
  const metric = input({ value: existing?.eventMetric ?? '', placeholder: '예: connections.total (비우면 전체)' });
  const connSelect = serverDbPicker({
    usable: conns.items.filter((i) => i.accessible),
    currentId: existing?.connectionId ?? '',
    onPick: () => {},
    serverLabel: '대상 서버',
    allLabel: '모든 커넥션',
    serverHelp: '비우면 어느 커넥션의 이벤트에도 반응합니다',
    inline: false,
  });
  const interval = input({
    type: 'number', value: String(existing?.minIntervalSec ?? 300), min: '10',
  });

  // 공통
  const skipBox = h('input', {
    type: 'checkbox', checked: existing ? existing.skipIfRunning : true,
  });
  // 파라미터는 고정값 또는 식 중 하나로 정한다.
  //
  // 자동 실행에서 넘기고 싶은 값은 대개 그때그때 다르다 — "방금 터진 이벤트의
  // 커넥션", "오늘 날짜". 고정값만 있으면 이벤트마다 트리거를 하나씩 만들어야 한다.
  const paramInputs = new Map();
  const paramFields = (macroParams ?? []).map((p) => {
    const savedExpr = existing?.paramExprs?.[p.name] ?? '';
    const saved = existing?.params?.[p.name];
    const control = paramControl(p, saved ?? p.default ?? '', conns);
    const exprInput = input({
      value: savedExpr,
      placeholder: '예: event.connectionId',
      autocomplete: 'off',
    });
    exprInput.classList.add('mono-input');

    const modeSelect = select([
      { value: 'value', label: '고정값' },
      { value: 'expr', label: 'Lua 식' },
    ], { value: savedExpr ? 'expr' : 'value' });

    const exprHelp = h('p.field-help', {},
      'trigger · event · now 를 쓸 수 있습니다. 예: event.connectionId, '
      + '"[" .. event.severity .. "] " .. event.message');
    const valueBox = h('div', {}, ...(control.nodes ?? [control]));
    const exprBox = h('div', {}, exprInput, exprHelp);

    const sync = () => {
      const isExpr = modeSelect.value === 'expr';
      valueBox.style.display = isExpr ? 'none' : '';
      exprBox.style.display = isExpr ? '' : 'none';
    };
    modeSelect.addEventListener('change', sync);
    sync();

    paramInputs.set(p.name, { control, exprInput, modeSelect, def: p });
    return h('div.field.trigger-param', {},
      h('span.field-label', {},
        p.label || p.name,
        p.required ? h('span.req-mark', { title: '필수' }, '*') : null,
        h('span.param-mode', {}, modeSelect),
      ),
      valueBox,
      exprBox,
      p.required ? h('p.field-help', {}, '필수 — 비우면 실행이 실패합니다') : null,
    );
  });

  const scheduleBox = h('div', {},
    field('주기', presetSelect),
    field('cron 식', cronInput, '분 시 일 월 요일 — 예: 0 3 * * * (매일 새벽 3시)'),
    field('시간대', tzInput, 'IANA 이름. 비우면 서버 지역 시간을 씁니다'),
    preview,
  );
  const eventBox = h('div', {},
    field('이벤트 종류', eventKind),
    field('심각도', severity, '이 이상인 이벤트에만 반응합니다'),
    field('지표', metric, '임계치 이벤트를 특정 지표로 좁힐 때'),
    ...connSelect.nodes,
    field('최소 간격(초)', interval,
      '같은 트리거가 연달아 터지는 것을 막습니다. 지표가 임계치 근처에서 흔들릴 때 필요합니다'),
  );

  const syncKind = () => {
    const isSchedule = kind.value === 'schedule';
    scheduleBox.style.display = isSchedule ? '' : 'none';
    eventBox.style.display = isSchedule ? 'none' : '';
    if (isSchedule) refreshPreview();
  };
  kind.addEventListener('change', syncKind);

  openModal({
    title: isEdit ? `트리거 수정 — ${existing.name}` : '자동 실행 트리거 추가',
    width: 640,
    body: () => {
      // 모달이 열린 뒤에 미리보기를 채운다(열기 전에는 요소가 문서에 없다).
      setTimeout(syncKind, 0);
      return [
        field('이름', name),
        field('종류', kind, isEdit ? '종류는 바꿀 수 없습니다. 다른 종류가 필요하면 새로 만드세요' : null),
        scheduleBox,
        eventBox,
        paramFields.length
          ? h('div.field', {},
              h('span.field-label', {}, '실행 파라미터'),
              h('div.trigger-params', {}, paramFields))
          : null,
        h('label.checkbox', {}, skipBox,
          h('span', {}, '이전 실행이 아직 진행 중이면 건너뛰기')),
        h('p.field-help', {},
          '켜 두는 것을 권합니다. 5분마다 도는 매크로가 6분 걸리기 시작하면 실행이 겹쳐 쌓입니다.'),
      ];
    },
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          if (!name.value.trim()) {
            toast('트리거 이름을 입력하세요', 'error');
            return;
          }
          const params = {};
          const paramExprs = {};
          for (const [key, { control, exprInput, modeSelect, def }] of paramInputs) {
            if (modeSelect.value === 'expr') {
              // 식으로 정한 파라미터는 고정값을 함께 보내지 않는다.
              // 둘 다 있으면 나중에 어느 쪽이 쓰였는지 화면에서 알 수 없다.
              paramExprs[key] = exprInput.value;
              continue;
            }
            if (def.type === 'boolean') params[key] = control.checked;
            else if (def.type === 'number') params[key] = Number(control.value);
            else params[key] = control.value;
          }
          const body = {
            name: name.value.trim(),
            kind: kind.value,
            params,
            paramExprs,
            skipIfRunning: skipBox.checked,
            cron: cronInput.value.trim(),
            timezone: tzInput.value.trim(),
            eventKind: eventKind.value,
            eventSeverity: severity.value,
            eventMetric: metric.value.trim(),
            connectionId: connSelect.value,
            minIntervalSec: Number(interval.value) || 300,
          };
          e.currentTarget.disabled = true;
          try {
            if (isEdit) await api.put(`/macros/triggers/${existing.id}`, body);
            else await api.post(`/macros/${macroID}/triggers`, body);
            close();
            toast(isEdit ? '트리거를 수정했습니다' : '트리거를 만들었습니다', 'success');
            reload();
          } catch (err) {
            toastError(err);
            e.currentTarget.disabled = false;
          }
        },
      }, isEdit ? '저장' : '만들기'),
    ],
  });
}

// paramControl은 매크로 파라미터 정의에 맞는 입력 요소를 만든다.
//
// 실행 대화상자와 형태를 맞춘 이유: 트리거는 "이 값으로 실행해 둔다"는 뜻이므로,
// 손으로 실행할 때와 다른 모양의 폼을 보면 같은 값을 넣는지 확신할 수 없다.
function paramControl(p, value, conns) {
  switch (p.type) {
    case 'boolean':
      return h('input', { type: 'checkbox', checked: Boolean(value) });
    case 'number':
      return input({ type: 'number', value: String(value ?? '') });
    case 'text':
      return h('textarea.input.textarea', { rows: 3 }, String(value ?? ''));
    case 'connection':
      return serverDbPicker({
        usable: conns.items.filter((i) => i.accessible),
        currentId: String(value ?? ''),
        onPick: () => {},
        allLabel: '선택 안 함',
        inline: false,
      });
    default:
      return input({ value: String(value ?? '') });
  }
}

// guessTimezone은 브라우저의 시간대를 기본값으로 제안한다.
// 사용자가 보고 있는 시계와 스케줄이 같은 기준이어야 헷갈리지 않는다.
function guessTimezone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone ?? '';
  } catch {
    return '';
  }
}

// ---------- 전체 목록 ----------

// renderTriggers는 모든 매크로의 자동 실행을 한 화면에 모은다.
export async function renderTriggers(outlet) {
  mount(outlet, spinner('자동 실행 목록을 불러오는 중…'));
  let res;
  try {
    res = await api.get('/macros/triggers');
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  mount(outlet,
    pageHeader('자동 실행', '정기 실행과 조건 실행을 한눈에', [
      h('a.btn', { href: '/macros' }, '← 매크로 목록'),
    ]),
    state.meta?.monitorEnabled === false
      ? h('p.notice.notice-warn', {}, icon('alert'),
          '모니터링이 꺼져 있어 조건(이벤트) 트리거는 동작하지 않습니다. 정기 실행은 그대로 돕니다.')
      : null,
    res.items.length === 0
      ? emptyState('자동 실행이 설정된 매크로가 없습니다.',
          h('a.btn', { href: '/macros' }, '매크로 목록으로'))
      : h('div.card', {},
          h('div.trigger-list', {}, res.items.map((item) => h('div.trigger-row', {},
            h('div.trigger-main', {},
              h('div.trigger-name', {},
                h('a', { href: `/macros/${item.trigger.macroId}` }, item.trigger.macroName || '(삭제됨)'),
                badge(item.trigger.name, 'neutral'),
                badge(item.trigger.kind === 'schedule' ? '정기' : '조건',
                  item.trigger.kind === 'schedule' ? 'accent' : 'info'),
                item.trigger.enabled ? null : badge('꺼짐', 'neutral'),
                item.trigger.lastStatus ? statusBadge(item.trigger.lastStatus) : null,
              ),
              h('div.trigger-when', {}, describeTrigger(item.trigger, item.describe)),
              h('div.muted.small', {},
                `${item.trigger.ownerName} 소유`,
                item.trigger.lastFiredAt ? ` · 마지막 ${relativeTime(item.trigger.lastFiredAt)}` : '',
                item.trigger.kind === 'schedule' && item.trigger.enabled && item.trigger.nextRunAt
                  ? ` · 다음 ${formatDate(item.trigger.nextRunAt)}` : '',
              ),
              item.trigger.lastError
                ? h('div.cell-sub.text-danger', {}, item.trigger.lastError) : null,
            ),
            h('a.btn.btn-small', { href: `/macros/${item.trigger.macroId}` }, '매크로 열기'),
          )))),
  );
}
