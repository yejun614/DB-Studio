// 매크로 목록과 실행 이력.
//
// **매크로는 만든 사람의 것이다.** 목록에는 내가 만든 것, 협업자로 초대된 것,
// 그리고 공개된 것만 나온다. 서버가 이미 걸러서 주므로 화면은 걸러진 목록을 그대로
// 그리고, 각 항목에 함께 온 권한(canEdit/canManage/canDelete)으로 버튼을 정한다.
//
// 권한을 화면에서 다시 계산하지 않는 이유: 규칙이 두 곳에 있으면 언젠가 갈라지고,
// 그 갈라짐은 "버튼은 있는데 누르면 403"이라는 형태로 나타난다.
import { api } from '../core/api.js';
import { state } from '../core/store.js';
import {
  h, mount, icon, input, textarea, select, spinner, emptyState, pageHeader,
  badge, toast, toastError, openModal, confirmDialog, field,
  formatDate, relativeTime,
} from '../core/ui.js';
import { navigate } from '../core/router.js';
import { codeEditor } from '../core/highlight.js';
import { errorPanel } from './users.js';
import { openShareDialog, visibilityBadge, accessLabel } from './macroshare.js';

// 이 화면은 탭 두 개다: 매크로와 커스텀 노드.
//
// 한 화면에 둔 이유: 커스텀 노드는 매크로를 만들다가 "이 부분은 매번 똑같네"라고
// 느낄 때 만들게 된다. 그런데 지금까지는 매크로 편집기 안에서만 만들 수 있어서,
// 이미 만들어 둔 노드를 고치거나 지우려면 아무 매크로나 열어야 했다.
// 노드는 매크로에 속한 것이 아니라 나란히 있는 것이므로, 목록도 나란히 둔다.
export async function renderMacros(outlet, params, query) {
  const tab = query?.get('tab') === 'nodes' ? 'nodes' : 'macros';
  mount(outlet, spinner('불러오는 중…'));

  let res;
  let defs = { items: [] };
  try {
    [res, defs] = await Promise.all([api.get('/macros/'), api.get('/macros/nodes')]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const reload = () => renderMacros(outlet, params, query);

  const tabs = h('div.tabs', {},
    tabButton('매크로', res.items.length, tab === 'macros', () => navigate('/macros')),
    tabButton('커스텀 노드', defs.items.length, tab === 'nodes', () => navigate('/macros?tab=nodes')),
  );

  mount(outlet,
    pageHeader('매크로', 'DB 자동화를 노드로 조립합니다', [
      h('a.btn', { href: '/macros/triggers' }, icon('refresh'), '자동 실행'),
      h('a.btn', { href: '/macros/runs' }, icon('history'), '실행 이력'),
      tab === 'nodes'
        ? h('button.btn.btn-primary', {
            type: 'button', onclick: () => openNodeDefDialog(null, reload),
          }, icon('plus'), '새 노드')
        : h('button.btn.btn-primary', {
            type: 'button', onclick: () => openCreateDialog(reload),
          }, icon('plus'), '새 매크로'),
    ]),
    state.permissions?.scriptRun && !state.meta?.shellEnabled
      ? h('p.notice.notice-warn', {}, icon('alert'),
          '셸 실행 권한이 있지만 서버가 -allow-shell 없이 실행 중이라 셸 노드는 동작하지 않습니다')
      : null,
    tabs,
    tab === 'nodes'
      ? nodeDefsView(defs.items, reload)
      : (res.items.length === 0
        ? emptyState('아직 매크로가 없습니다. 새로 만들어 보세요.',
            h('button.btn.btn-primary', { type: 'button', onclick: () => openCreateDialog(reload) }, '새 매크로'))
        : h('div.macro-grid', {}, res.items.map((m) => macroCard(m, reload)))),
  );
}

// DEFAULT_NODE_SCRIPT는 새 노드의 출발점이다.
// 빈 편집기를 주면 "여기서 무엇을 쓸 수 있는가"부터 찾아야 한다.
const DEFAULT_NODE_SCRIPT = `-- vars: 실행 문맥의 변수, params: 이 노드의 설정값
-- log.info / db.query / sh.run 등을 쓸 수 있습니다(실행자 권한으로 검사됩니다).
log.info("사용자 노드 실행")
return { ok = true }`;

// openNodeDefDialog는 목록 화면에서 커스텀 노드를 만들거나 고친다.
//
// 매크로 편집기 안의 같은 이름 대화상자와 다른 점은 하나다: 여기에는 "이 매크로"라는
// 문맥이 없으므로 **전역 노드만** 만든다. 매크로 전용 노드는 그 매크로를 열어야
// 만들 수 있고(그것이 전용이라는 뜻이다), 여기서는 고치고 지우는 것까지만 한다.
function openNodeDefDialog(existing, reload) {
  const canEdit = existing ? existing.canEdit : true;
  const name = input({ value: existing?.name ?? '', placeholder: '슬랙 알림', disabled: !canEdit });
  const description = input({ value: existing?.description ?? '', disabled: !canEdit });
  const ports = input({
    value: (safeJSON(existing?.ports, []) ?? []).join(', '),
    placeholder: 'out',
    disabled: !canEdit,
  });
  const fields = codeEditor({
    value: existing ? prettyJSON(existing.fields) : '[]',
    language: 'json', rows: 5,
  });
  const script = codeEditor({
    value: existing?.script ?? DEFAULT_NODE_SCRIPT,
    language: 'lua', rows: 14,
  });
  const note = input({ placeholder: '무엇을 바꿨는지' });

  // 코드 편집기에는 disabled 개념이 없다(textarea 위에 하이라이트를 겹쳐 그린다).
  // 읽기 전용은 textarea에 직접 건다 — 선택과 복사는 그대로 되고 편집만 막힌다.
  if (!canEdit) {
    for (const ed of [fields, script]) {
      ed.el.querySelector('textarea')?.setAttribute('readonly', 'readonly');
    }
  }

  const save = async (close) => {
    let parsedFields;
    try {
      parsedFields = JSON.parse(fields.value || '[]');
    } catch (err) {
      toast(`설정 필드 JSON을 읽을 수 없습니다: ${err.message}`, 'error');
      return;
    }
    const body = {
      name: name.value.trim(),
      description: description.value.trim(),
      // 기존 노드를 고칠 때는 범위를 그대로 둔다. 매크로 전용 노드를 여기서
      // 전역으로 바꿔 버리면 그 매크로 밖에서도 보이게 되어 공개 범위가 넓어진다.
      scope: existing?.scope ?? 'global',
      macroId: existing?.macroId ?? '',
      ports: ports.value.split(',').map((s) => s.trim()).filter(Boolean),
      fields: parsedFields,
      script: script.value,
      note: note.value.trim(),
    };
    try {
      if (existing) await api.put(`/macros/nodes/${encodeURIComponent(existing.id)}`, body);
      else await api.post('/macros/nodes', body);
      close();
      toast('노드를 저장했습니다', 'success');
      reload();
    } catch (err) {
      toastError(err);
    }
  };

  openModal({
    title: existing ? `커스텀 노드 — ${existing.name}` : '커스텀 노드 만들기',
    width: 780,
    body: () => [
      existing && !canEdit
        ? h('p.notice.notice-info', {}, icon('lock'),
            existing.access === 'view'
              ? '이 노드는 조회만 허용되어 있습니다.'
              : '이 노드를 수정할 권한이 없습니다.')
        : null,
      existing?.scope === 'macro'
        ? h('p.notice.notice-info', {}, icon('alert'),
            `${existing.macroName || '한 매크로'} 전용 노드입니다. 다른 매크로에서는 보이지 않습니다.`)
        : null,
      field('이름', name),
      field('설명', description),
      field('출력 포트', ports, '쉼표로 구분합니다. 비워두면 out 하나입니다'),
      field('설정 필드 (JSON)', fields.el,
        '예: [{"key":"url","label":"주소","type":"text"}] — 값은 스크립트에서 params.url 로 읽습니다'),
      field('Lua 스크립트', script.el,
        'vars 로 변수를, params 로 설정값을 읽습니다. return 값이 노드 결과이고, 두 번째 반환값으로 포트를 고릅니다'),
      existing && canEdit ? field('변경 메모', note) : null,
      existing?.scope === 'global' && existing.canManage
        ? h('div.form-actions', {},
            h('button.btn.btn-small', {
              type: 'button',
              onclick: () => openShareDialog(
                { kind: 'node', id: existing.id, name: existing.name, item: existing }, reload),
            }, icon('users'), '공유'))
        : null,
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, canEdit ? '취소' : '닫기'),
      canEdit
        ? h('button.btn.btn-primary', { type: 'button', onclick: () => save(close) }, '저장')
        : null,
    ].filter(Boolean),
  });
}

function safeJSON(raw, fallback) {
  if (!raw) return fallback;
  if (typeof raw !== 'string') return raw;
  try {
    return JSON.parse(raw);
  } catch {
    return fallback;
  }
}

function prettyJSON(raw) {
  return JSON.stringify(safeJSON(raw, []), null, 2);
}

function tabButton(label, count, active, onclick) {
  // 활성 표시는 로그 화면의 탭과 같은 클래스를 쓴다(.tab-active).
  // 같은 모양의 것을 두 벌로 두면 한쪽만 손보게 된다.
  return h('button.tab', {
    type: 'button', class: active ? 'tab-active' : '', onclick,
  }, label, h('span.tab-count', {}, String(count)));
}

// nodeDefsView는 커스텀 노드 목록이다.
//
// 매크로 전용 노드도 함께 보여주되 어느 매크로의 것인지 적는다. 목록에서 감추면
// "만들었는데 어디에도 없다"가 되고, 구분 없이 섞으면 전역 노드인 줄 알고 다른
// 매크로에서 찾게 된다.
function nodeDefsView(items, reload) {
  if (items.length === 0) {
    return emptyState(
      '커스텀 노드가 없습니다. Lua로 만든 노드는 모든 매크로에서 부품처럼 씁니다.',
      h('button.btn.btn-primary', {
        type: 'button', onclick: () => openNodeDefDialog(null, reload),
      }, '새 노드'));
  }
  return h('div.nodedef-grid', {}, items.map((d) => h('div.card.nodedef-card', {},
    h('div.macro-card-head', {},
      h('button.macro-name.as-link', {
        type: 'button', onclick: () => openNodeDefDialog(d, reload),
      }, d.name),
      badge(`v${d.currentVersion}`, 'neutral'),
      badge(d.scope === 'global' ? '전역' : '매크로 전용', d.scope === 'global' ? 'info' : 'neutral'),
      d.scope === 'global' ? visibilityBadge(d) : null,
    ),
    d.description ? h('p.macro-desc', {}, d.description) : null,
    h('div.macro-meta', {},
      d.scope === 'macro' && d.macroName ? h('span', {}, `${d.macroName} 전용`) : null,
      h('span', {}, `${d.createdByName || '알 수 없음'} 작성`),
      h('span', {}, accessLabel(d.access)),
      h('span', {}, `수정 ${relativeTime(d.updatedAt)}`),
    ),
    h('div.macro-card-actions', {},
      h('button.btn.btn-small', {
        type: 'button', onclick: () => openNodeDefDialog(d, reload),
      }, icon(d.canEdit ? 'edit' : 'list'), d.canEdit ? '수정' : '보기'),
      d.canDelete
        ? h('button.btn.btn-small.btn-danger-ghost', {
            type: 'button',
            onclick: async () => {
              const ok = await confirmDialog({
                title: '노드 삭제',
                message: `"${d.name}" 노드를 삭제합니다. 이 노드를 쓰고 있는 매크로는 실행할 때 오류가 납니다.`,
                confirmLabel: '삭제', danger: true,
              });
              if (!ok) return;
              try {
                await api.del(`/macros/nodes/${encodeURIComponent(d.id)}`);
                toast('노드를 삭제했습니다', 'success');
                reload();
              } catch (err) { toastError(err); }
            },
          }, icon('trash'), '삭제')
        : null,
    ),
  )));
}

function macroCard(m, reload) {
  return h('div.card.macro-card', {},
    h('div.macro-card-head', {},
      h('a.macro-name', { href: `/macros/${m.id}` }, m.name),
      badge(`v${m.currentVersion}`, 'neutral'),
      visibilityBadge(m),
      m.lastRunStatus ? runStatusBadge(m.lastRunStatus) : null,
    ),
    m.description ? h('p.macro-desc', {}, m.description) : null,
    h('div.macro-meta', {},
      h('span', {}, `${m.versionCount}개 버전`),
      h('span', {}, `${m.createdByName || '알 수 없음'} 작성`),
      // 내 위치를 함께 적는다. 열기 버튼을 눌러 저장 버튼이 없는 것을 보고서야
      // "남의 매크로였구나"를 알게 되면 늦다.
      h('span', {}, accessLabel(m.access)),
      m.collaboratorCount ? h('span', {}, `협업자 ${m.collaboratorCount}명`) : null,
      h('span', {}, m.lastRunAt ? `마지막 실행 ${relativeTime(m.lastRunAt)}` : '실행 기록 없음'),
      h('span', {}, `수정 ${relativeTime(m.updatedAt)}${m.updatedByName ? ` · ${m.updatedByName}` : ''}`),
    ),
    h('div.macro-card-actions', {},
      h('a.btn.btn-small', { href: `/macros/${m.id}` },
        icon(m.canEdit ? 'edit' : 'list'), m.canEdit ? '열기' : '보기'),
      h('a.btn.btn-small', { href: `/macros/runs?macro=${encodeURIComponent(m.id)}` },
        icon('history'), '이력'),
      m.canManage
        ? h('button.btn.btn-small', {
            type: 'button',
            onclick: () => openShareDialog(
              { kind: 'macro', id: m.id, name: m.name, item: m }, reload),
          }, icon('users'), '공유')
        : null,
      m.canDelete
        ? h('button.btn.btn-small.btn-danger-ghost', {
            type: 'button',
            onclick: async () => {
              const ok = await confirmDialog({
                title: '매크로 삭제',
                message: `${m.name} 과(와) 모든 버전을 삭제합니다. 실행 이력은 기록으로 남습니다.`,
                confirmLabel: '삭제', danger: true, requireText: m.name,
              });
              if (!ok) return;
              try {
                await api.del(`/macros/${m.id}`);
                toast('매크로를 삭제했습니다', 'success');
                reload();
              } catch (err) {
                toastError(err);
              }
            },
          }, icon('trash'), '삭제')
        : null,
    ),
  );
}

function openCreateDialog(reload) {
  const name = input({ placeholder: '야간 정리 작업', autocomplete: 'off' });
  const description = textarea({ rows: 2, placeholder: '무엇을 하는 매크로인지 적어두면 나중에 찾기 쉽습니다' });

  openModal({
    title: '새 매크로',
    body: () => [
      field('이름', name),
      field('설명', description),
      h('p.field-help', {},
        '만들면 시작 노드 하나가 있는 빈 매크로가 생깁니다. 이어서 노드를 붙여 나가세요.'),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async () => {
          if (!name.value.trim()) {
            toast('이름을 입력하세요', 'error');
            return;
          }
          try {
            const res = await api.post('/macros/', {
              name: name.value.trim(),
              description: description.value.trim(),
            });
            close();
            navigate(`/macros/${res.macro.id}`);
          } catch (err) {
            toastError(err);
          }
        },
      }, '만들기'),
    ],
  });
}

// ---------- 실행 이력 ----------

export async function renderMacroRuns(outlet, params, query) {
  mount(outlet, spinner('실행 이력을 불러오는 중…'));

  const macroFilter = query.get('macro') ?? '';
  const statusFilter = query.get('status') ?? '';

  let res;
  let macros;
  try {
    [res, macros] = await Promise.all([
      api.get(`/macros/runs?macro=${encodeURIComponent(macroFilter)}&status=${encodeURIComponent(statusFilter)}&limit=200`),
      api.get('/macros/'),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const macroSelect = select(
    [{ value: '', label: '모든 매크로' },
      ...macros.items.map((m) => ({ value: m.id, label: m.name }))],
    { value: macroFilter },
  );
  const statusSelect = select([
    { value: '', label: '모든 상태' },
    { value: 'running', label: '실행 중' },
    { value: 'success', label: '성공' },
    { value: 'failed', label: '실패' },
    { value: 'canceled', label: '취소' },
  ], { value: statusFilter });

  const apply = () => {
    const parts = [];
    if (macroSelect.value) parts.push(`macro=${encodeURIComponent(macroSelect.value)}`);
    if (statusSelect.value) parts.push(`status=${encodeURIComponent(statusSelect.value)}`);
    navigate(`/macros/runs${parts.length ? `?${parts.join('&')}` : ''}`);
  };
  macroSelect.addEventListener('change', apply);
  statusSelect.addEventListener('change', apply);

  mount(outlet,
    pageHeader('매크로 실행 이력', '성공·실패·취소로 분류된 실행 기록', [
      h('a.btn', { href: '/macros' }, '← 매크로 목록'),
    ]),
    h('div.card.filter-bar', {}, macroSelect, statusSelect),
    res.items.length === 0
      ? emptyState('실행 기록이 없습니다')
      : h('div.card.table-scroll', {},
          h('table.table', {},
            h('thead', {}, h('tr', {},
              h('th', {}, '상태'),
              h('th', {}, '매크로'),
              h('th', {}, '실행자'),
              h('th', {}, '시작'),
              h('th', {}, '소요'),
              h('th', {}, ''),
            )),
            h('tbody', {}, res.items.map((r) => h('tr', {},
              h('td', {}, runStatusBadge(r.status)),
              h('td', {},
                h('div.cell-main', {}, r.macroName || '(삭제된 매크로)', badge(`v${r.version}`, 'neutral')),
                r.error ? h('div.cell-sub.text-danger', {}, r.error) : null,
                r.trigger === 'macro' ? h('div.cell-sub', {}, '다른 매크로가 호출') : null,
              ),
              h('td', {}, h('div.cell-main', {}, r.actorName),
                r.actorIp ? h('div.cell-sub', {}, r.actorIp) : null),
              h('td', {}, h('div.cell-main', {}, formatDate(r.startedAt)),
                h('div.cell-sub', {}, relativeTime(r.startedAt))),
              h('td', {}, r.status === 'running' ? '진행 중' : `${(r.durationMs / 1000).toFixed(1)}초`),
              h('td', {},
                h('button.btn.btn-small', {
                  type: 'button', onclick: () => openRunLog(r.id),
                }, icon('list'), '로그'),
              ),
            ))),
          )),
  );
}

export function runStatusBadge(status) {
  const map = {
    running: ['실행 중', 'info'],
    success: ['성공', 'success'],
    failed: ['실패', 'danger'],
    canceled: ['취소', 'neutral'],
  };
  const [label, kind] = map[status] ?? [status, 'neutral'];
  return badge(label, kind);
}

// openRunLog는 실행 로그를 모달로 보여준다.
// 실행 중이면 스트림을 붙여 실시간으로 이어 그린다.
export function openRunLog(runID, onClose) {
  const logBox = h('div.run-log');
  const headBox = h('div.run-log-head');
  let stream = null;

  const close = openModal({
    title: '실행 로그',
    width: 860,
    body: () => h('div', {}, headBox, logBox),
    onClose: () => {
      stream?.close();
      onClose?.();
    },
  });

  const appendLog = (entry) => {
    logBox.appendChild(logLine(entry));
    logBox.scrollTop = logBox.scrollHeight;
  };

  const drawHead = (run, live) => {
    mount(headBox,
      h('div.run-summary', {},
        runStatusBadge(run.status),
        h('span', {}, `${run.macroName} v${run.version}`),
        h('span.muted', {}, `${run.actorName} · ${formatDate(run.startedAt)}`),
        run.status !== 'running'
          ? h('span.muted', {}, `${(run.durationMs / 1000).toFixed(1)}초 · 노드 ${run.nodeCount}개`)
          : null,
        live
          ? h('button.btn.btn-small.btn-danger', {
              type: 'button',
              onclick: async () => {
                try {
                  await api.post(`/macros/runs/${runID}/cancel`);
                  toast('취소를 요청했습니다', 'info');
                } catch (err) {
                  toastError(err);
                }
              },
            }, icon('stop'), '실행 취소')
          : null,
      ),
      run.error ? h('p.notice.notice-danger', {}, icon('alert'), run.error) : null,
    );
  };

  (async () => {
    let res;
    try {
      res = await api.get(`/macros/runs/${runID}`);
    } catch (err) {
      mount(logBox, errorPanel(err));
      return;
    }
    drawHead(res.run, res.live);
    mount(logBox);
    for (const entry of res.logs) appendLog(entry);

    if (!res.live) return;

    // 실행 중이면 스트림을 붙인다. 이미 받은 줄은 after로 걸러 중복을 막는다.
    const lastSeq = res.logs.length ? res.logs[res.logs.length - 1].seq : 0;
    stream = new EventSource(`/api/v1/macros/runs/${runID}/stream?after=${lastSeq}`);
    stream.addEventListener('log', (e) => appendLog(JSON.parse(e.data)));
    stream.addEventListener('done', (e) => {
      drawHead(JSON.parse(e.data), false);
      stream.close();
      stream = null;
    });
    stream.addEventListener('error', () => {
      // 스트림이 끊기면 조용히 닫는다. 로그는 DB에 남아 있으므로
      // 모달을 다시 열면 전부 보인다 — 여기서 재연결을 시도하면
      // 이미 끝난 실행에 계속 붙으려 하게 된다.
      stream?.close();
      stream = null;
    });
  })();

  return close;
}

function logLine(entry) {
  return h(`div.run-log-line.log-${entry.level}`, {},
    h('span.log-time', {}, new Date(entry.at).toLocaleTimeString()),
    entry.node ? h('span.log-node', {}, entry.node) : null,
    h('span.log-message', {}, entry.message),
    entry.detail ? detailBlock(entry.detail) : null,
  );
}

function detailBlock(detail) {
  const text = Object.entries(detail)
    .filter(([, v]) => v !== null && v !== undefined && v !== '')
    .map(([k, v]) => `${k}: ${typeof v === 'string' ? v : JSON.stringify(v)}`)
    .join('\n');
  if (!text) return null;
  return h('details.log-detail', {}, h('summary', {}, '자세히'), h('pre', {}, text));
}
