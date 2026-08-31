// 사용자별 DB 접근 권한 설정 화면.
// 접근 범위(mode) × 커넥션별 능력 등급을 한 화면에서 편집한다.
import { api } from '../core/api.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, spinner, emptyState, pageHeader,
  toast, toastError, badge, envBadge, levelBadge, roleBadge,
} from '../core/ui.js';
import { dbLogo } from '../core/dblogo.js';
import { errorPanel } from './users.js';

export async function renderAccess(outlet, params) {
  mount(outlet, spinner());
  let data;
  try {
    data = await api.get(`/users/${params.id}/access`);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const { user, policy, connections, servers, projects } = data;
  const levels = state.meta?.levels ?? [];
  const modes = state.meta?.accessModes ?? [];
  const caps = state.meta?.capabilities ?? [];
  const permDefs = state.meta?.perms ?? [];

  // 편집 중 상태. 저장 버튼을 누를 때까지 서버에 반영하지 않는다.
  const draft = {
    mode: policy.mode,
    defaultLevel: policy.defaultLevel,
    items: new Set(policy.items),
    capabilities: new Map(Object.entries(policy.capabilities ?? {})),
    // 데이터 능력. 등급과 독립적인 축이므로 별도의 기본값과 오버라이드를 가진다.
    defaultCaps: new Set(policy.defaultCaps ?? []),
    capOverrides: new Map(
      Object.entries(policy.capOverrides ?? {}).map(([id, list]) => [id, new Set(list)])),
    // 서버 단위 일괄 부여. DB 지정이 있으면 그쪽이 이긴다(서버 판정과 같은 규칙).
    serverItems: new Set(policy.serverItems ?? []),
    serverCapabilities: new Map(Object.entries(policy.serverCapabilities ?? {})),
    serverCapOverrides: new Map(
      Object.entries(policy.serverCapOverrides ?? {}).map(([id, list]) => [id, new Set(list)])),
    // 전역 권한. 커넥션에 매이지 않지만 권한을 두 화면에 나눠 두면 어느 쪽이
    // 최신인지 알 수 없으므로 여기서 함께 편집하고 함께 저장한다.
    perms: new Set(user.perms ?? []),
    // 참여 프로젝트. 등급보다 앞선 관문이라 이 화면에서 함께 정한다 — 등급을
    // 아무리 줘도 참여하지 않았으면 그 DB는 목록에조차 나오지 않는다.
    projects: new Set(policy.projects ?? []),
  };

  // 서버별로 DB를 묶는다. 서버가 없는 DB는 없지만, 목록이 어긋난 경우에도
  // 화면에서 사라지지 않도록 남는 것들을 따로 모은다.
  const byServer = new Map((servers ?? []).map((srv) => [srv.id, { server: srv, dbs: [] }]));
  const orphans = [];
  for (const conn of connections) {
    const group = byServer.get(conn.serverId);
    if (group) group.dbs.push(conn);
    else orphans.push(conn);
  }
  const groups = [...byServer.values()].filter((g) => g.dbs.length > 0);
  if (orphans.length) groups.push({ server: null, dbs: orphans });

  const isSuperadmin = user.role === 'superadmin';

  const modeSelect = select(modes.map((m) => ({ value: m.value, label: m.label })), {
    value: draft.mode,
    disabled: isSuperadmin,
  });
  const defaultLevelSelect = select(levels.map((l) => ({ value: l.value, label: l.label })), {
    value: draft.defaultLevel,
    disabled: isSuperadmin,
  });

  const tableBox = h('div.card');
  const summaryBox = h('div.access-summary');
  const defaultCapsBox = h('div.cap-pick');

  // 전역 권한 토글. 등급·데이터 능력과 달리 DB 목록에 영향을 주지 않으므로
  // rebuild()를 걸지 않는다(요약 칩은 DB 기준 집계다).
  const permBoxes = permDefs.map((p) => {
    // 서버가 -allow-shell 없이 떠 있으면 권한만 켤 수 있어도 실행되지 않는다.
    // 이유를 체크박스 옆에 적어 관리자가 서버 설정을 찾아가게 한다.
    const shellOff = p.value === 'script.run' && !state.meta?.shellEnabled;
    return h('label.cap-toggle', {},
      h('input', {
        type: 'checkbox',
        checked: draft.perms.has(p.value),
        disabled: shellOff || isSuperadmin,
        onchange: (e) => {
          if (e.target.checked) draft.perms.add(p.value);
          else draft.perms.delete(p.value);
        },
      }),
      h('span', {}, p.label),
      h('span.field-help', {},
        shellOff ? '서버가 -allow-shell 없이 실행 중이라 사용할 수 없습니다' : (p.help ?? '')),
    );
  });

  const rebuild = () => {
    mount(tableBox, connectionTable(groups, draft, levels, caps, isSuperadmin, rebuild));
    mount(summaryBox, ...summaryChips(connections, draft, caps, isSuperadmin));
    mount(defaultCapsBox, ...caps.map((cap) => capToggle(cap, draft.defaultCaps, isSuperadmin, () => {
      normalizeCapSet(draft.defaultCaps);
      rebuild();
    })));
  };

  modeSelect.addEventListener('change', () => {
    draft.mode = modeSelect.value;
    rebuild();
  });
  defaultLevelSelect.addEventListener('change', () => {
    draft.defaultLevel = defaultLevelSelect.value;
    rebuild();
  });

  const saveBtn = h('button.btn.btn-primary', { type: 'button', disabled: isSuperadmin },
    icon('check'), '권한 저장');
  saveBtn.addEventListener('click', async () => {
    saveBtn.disabled = true;
    try {
      await api.put(`/users/${user.id}/access`, {
        mode: draft.mode,
        defaultLevel: draft.defaultLevel,
        items: [...draft.items],
        capabilities: Object.fromEntries(draft.capabilities),
        defaultCaps: [...draft.defaultCaps],
        capOverrides: Object.fromEntries(
          [...draft.capOverrides].map(([id, set]) => [id, [...set]])),
        serverItems: [...draft.serverItems],
        serverCapabilities: Object.fromEntries(draft.serverCapabilities),
        serverCapOverrides: Object.fromEntries(
          [...draft.serverCapOverrides].map(([id, set]) => [id, [...set]])),
        perms: [...draft.perms],
        projects: [...draft.projects],
      });
      toast('권한을 저장했습니다. 해당 사용자의 세션이 초기화됩니다.', 'success');
      renderAccess(outlet, params);
    } catch (err) {
      toastError(err);
      saveBtn.disabled = false;
    }
  });

  mount(outlet,
    pageHeader(
      `권한 설정 — ${user.username}`,
      user.displayName || user.email || '',
      [h('a.btn', { href: '/users' }, '← 사용자 목록'), saveBtn],
    ),
    h('div.card.access-head', {},
      h('div.access-user', {},
        roleBadge(user.role),
        user.status === 'active' ? badge('활성', 'success') : badge('비활성', 'neutral'),
      ),
      isSuperadmin
        ? h('p.notice.notice-info', {}, icon('shield'),
            '슈퍼 어드민은 모든 DB에 대해 마이그레이션까지 항상 허용되고 전역 권한도 모두 가집니다. '
            + '권한 설정 대상이 아닙니다.')
        : [
            // 참여 프로젝트를 맨 위에 둔다. 아래의 모든 설정보다 앞선 관문이라
            // 여기서 빠져 있으면 그 아래에 무엇을 적어도 효력이 없다.
            h('div.field.access-global', {},
              h('span.field-label', {}, '참여 프로젝트'),
              h('div.cap-pick.cap-pick-stacked', {},
                (projects ?? []).length === 0
                  ? h('span.muted.small', {}, '아직 만들어진 프로젝트가 없습니다')
                  : (projects ?? []).map((p) => h('label.cap-toggle', {},
                    h('input', {
                      type: 'checkbox',
                      checked: draft.projects.has(p.id),
                      disabled: isSuperadmin,
                      onchange: (e) => {
                        if (e.target.checked) draft.projects.add(p.id);
                        else draft.projects.delete(p.id);
                      },
                    }),
                    h('span', {}, p.name),
                    h('span.field-help', {},
                      `서버 ${p.servers}개 · DB ${p.connections}개 · ERD ${p.documents}개`)))),
              h('span.field-help', {},
                '아래 설정보다 앞선 관문입니다. 참여하지 않은 프로젝트의 DB는 '
                + '등급이 무엇으로 적혀 있든 목록에도 나오지 않습니다'),
            ),
            // 전역 권한을 그다음에 둔다. DB에 매이지 않는 권한이므로 아래의
            // "이 DB에 무엇을 허용할까"와 섞이면 커넥션별 설정으로 오해한다.
            h('div.field.access-global', {},
              h('span.field-label', {}, '전역 권한'),
              h('div.cap-pick.cap-pick-stacked', {}, permBoxes),
              h('span.field-help', {},
                '특정 DB에 매이지 않는 기능 권한입니다. 매크로는 여러 DB에 걸쳐 동작하고 '
                + '셸 스크립트는 DB와 무관하게 서버에서 실행됩니다'),
            ),
            h('div.form-grid', {},
              h('label.field', {}, h('span.field-label', {}, '접근 범위'), modeSelect,
                h('span.field-help', {}, modeHelp(draft.mode))),
              h('label.field', {}, h('span.field-label', {}, '기본 능력 등급'), defaultLevelSelect,
                h('span.field-help', {}, '서버에도 DB에도 따로 지정하지 않았을 때 적용됩니다')),
            ),
            // 데이터 능력을 등급 아래 별도 블록으로 둔다. 같은 줄에 놓으면
            // 두 축이 하나의 사다리처럼 읽혀서, 등급을 올리면 데이터도 열린다고
            // 오해하게 된다.
            h('div.field', {},
              h('span.field-label', {}, '기본 데이터 능력'),
              defaultCapsBox,
              h('span.field-help', {},
                '등급과 별개의 축입니다. 마이그레이션 권한이 있어도 여기서 주지 않으면 데이터는 볼 수 없습니다'),
            ),
          ],
      summaryBox,
    ),
      groups.length === 0 ? emptyState('등록된 DB가 없습니다') : tableBox,
  );

  rebuild();
}

function modeHelp(mode) {
  switch (mode) {
    case 'all': return '등록된 모든 DB에 접근할 수 있습니다 (선택 목록 무시)';
    case 'allowlist': return '아래에서 체크한 DB에만 접근할 수 있습니다';
    case 'denylist': return '아래에서 체크한 DB만 접근이 차단되고 나머지는 허용됩니다';
    default: return '';
  }
}

// isInScope는 현재 draft 기준으로 접근 가능 여부를 계산한다.
//
// 서버의 resolveWithPolicy와 같은 규칙이어야 한다. 저장하기 전에 결과를 보여주는 것이
// 이 화면의 요점인데, 두 규칙이 갈라지면 화면이 보여준 것과 실제가 달라진다.
function isInScope(draft, conn) {
  const checked = draft.items.has(conn.id);
  const serverChecked = Boolean(conn.serverId) && draft.serverItems.has(conn.serverId);
  switch (draft.mode) {
    case 'all': return true;
    // 서버가 목록에 있으면 그 아래 DB도 목록에 있는 것으로 본다.
    case 'allowlist': return checked || serverChecked;
    // 차단은 넓게 걸린다 — 어느 한쪽이라도 목록에 있으면 막힌다.
    case 'denylist': return !checked && !serverChecked;
    default: return false;
  }
}

function effectiveLevel(draft, conn) {
  if (!isInScope(draft, conn)) return 'none';
  if (draft.capabilities.has(conn.id)) return draft.capabilities.get(conn.id);
  if (conn.serverId && draft.serverCapabilities.has(conn.serverId)) {
    return draft.serverCapabilities.get(conn.serverId);
  }
  return draft.defaultLevel;
}

// effectiveCaps는 이 DB에 실제로 적용될 능력 집합이다.
// 범위 밖이면 등급과 마찬가지로 아무것도 적용되지 않는다.
function effectiveCaps(draft, conn) {
  if (!isInScope(draft, conn)) return new Set();
  if (draft.capOverrides.has(conn.id)) return draft.capOverrides.get(conn.id);
  // DB 하나만 "없음"으로 내린 것은 예외를 빼겠다는 뜻이므로 물려받은 능력도 지운다
  // (서버 판정과 같은 규칙).
  if (draft.capabilities.get(conn.id) === 'none') return new Set();
  if (conn.serverId && draft.serverCapOverrides.has(conn.serverId)) {
    return draft.serverCapOverrides.get(conn.serverId);
  }
  return draft.defaultCaps;
}

// normalizeCapSet은 서버의 normalizeCaps와 같은 규칙을 화면에서도 지킨다:
// 수정 권한은 조회 권한을 함께 요구한다. 저장할 때 400으로 거절하는 대신
// 체크하는 순간 조회를 함께 켜서, 사용자가 규칙을 눌러 보면서 알게 한다.
function normalizeCapSet(set) {
  if (set.has('data.write')) set.add('data.read');
  return set;
}

function capToggle(cap, set, disabled, onChange) {
  const box = h('input', {
    type: 'checkbox',
    checked: set.has(cap.value),
    disabled,
    onchange: (e) => {
      if (e.target.checked) set.add(cap.value);
      else {
        set.delete(cap.value);
        // 조회를 끄면 수정도 함께 꺼진다 — 남겨두면 저장이 거부되는 조합이 된다.
        if (cap.value === 'data.read') set.delete('data.write');
      }
      onChange();
    },
  });
  return h('label.cap-toggle', { title: cap.help ?? '' }, box, h('span', {}, cap.label));
}

function summaryChips(connections, draft, caps, isSuperadmin) {
  const counts = { none: 0, monitor: 0, erd: 0, migrate: 0 };
  const capCounts = new Map(caps.map((c) => [c.value, 0]));
  for (const c of connections) {
    const level = isSuperadmin ? 'migrate' : effectiveLevel(draft, c);
    counts[level] = (counts[level] ?? 0) + 1;
    const active = isSuperadmin ? new Set(caps.map((x) => x.value)) : effectiveCaps(draft, c);
    for (const value of active) capCounts.set(value, (capCounts.get(value) ?? 0) + 1);
  }
  return [
    h('div.summary-row', {},
      h('span.summary-label', {}, '실효 권한 요약'),
      chip('접근 불가', counts.none, 'neutral'),
      chip('모니터링', counts.monitor, 'info'),
      chip('ERD 설계', counts.erd, 'accent'),
      chip('마이그레이션', counts.migrate, 'warn'),
    ),
    h('div.summary-row', {},
      h('span.summary-label', {}, '데이터 능력 요약'),
      ...caps.map((c) => chip(c.label, capCounts.get(c.value) ?? 0,
        c.value === 'data.read' ? 'info' : 'warn')),
    ),
  ];
}

function chip(label, count, kind) {
  return h(`span.chip.chip-${kind}`, {}, label, h('b', {}, count));
}

function connectionTable(groups, draft, levels, caps, isSuperadmin, rebuild) {
  const showCheckbox = draft.mode !== 'all' && !isSuperadmin;
  const checkboxLabel = draft.mode === 'denylist' ? '차단' : '허용';

  // levelPicker는 서버 줄과 DB 줄이 같은 모양의 선택기를 쓰게 한다.
  // 두 줄이 다르게 생기면 "이 값이 아래로 물려지는가"를 매번 다시 판단해야 한다.
  const levelPicker = (map, id, inheritLabel, disabled) => select(
    [{ value: '', label: `상속 (${inheritLabel})` },
      ...levels.map((l) => ({ value: l.value, label: l.label }))],
    {
      value: map.has(id) ? map.get(id) : '',
      disabled,
      onchange: (e) => {
        if (e.target.value === '') map.delete(id);
        else map.set(id, e.target.value);
        rebuild();
      },
    },
  );

  const capCell = (overrideMap, id, shown, disabled, inheritNote) => {
    const overridden = overrideMap.has(id);
    return h('div.cap-cell', {},
      ...caps.map((cap) => capToggle(cap, shown, disabled, () => {
        normalizeCapSet(shown);
        overrideMap.set(id, shown);
        rebuild();
      })),
      overridden && !isSuperadmin
        ? h('button.link-btn', {
            type: 'button', title: inheritNote,
            onclick: () => { overrideMap.delete(id); rebuild(); },
          }, '상속으로')
        : null,
    );
  };

  const defaultLevelLabel = levels.find((l) => l.value === draft.defaultLevel)?.label
    ?? draft.defaultLevel;
  const rows = [];

  for (const { server, dbs } of groups) {
    if (server) {
      const srvLevel = draft.serverCapabilities.get(server.id);
      const srvShown = isSuperadmin
        ? new Set(caps.map((x) => x.value))
        : new Set(draft.serverCapOverrides.get(server.id) ?? draft.defaultCaps);

      rows.push(h('tr.access-server-row', {},
        showCheckbox ? h('td.col-check', {}, h('input', {
          type: 'checkbox',
          checked: draft.serverItems.has(server.id),
          onchange: (e) => {
            if (e.target.checked) draft.serverItems.add(server.id);
            else draft.serverItems.delete(server.id);
            rebuild();
          },
        })) : null,
        h('td', {},
          h('div.cell-main', {}, dbLogo(server.kind, 15), server.name,
            badge(`DB ${dbs.length}개`, 'neutral')),
          h('div.cell-sub', {},
            `${kindLabel(server.kind)} · ${server.kind === 'sqlite' ? '파일' : `${server.host}:${server.port}`}`),
        ),
        h('td', {}, levelPicker(draft.serverCapabilities, server.id, defaultLevelLabel, isSuperadmin)),
        h('td', {}, capCell(draft.serverCapOverrides, server.id, srvShown, isSuperadmin,
          '이 서버의 데이터 능력을 기본값으로 되돌립니다')),
        h('td', {}, srvLevel || draft.serverItems.has(server.id) || draft.serverCapOverrides.has(server.id)
          ? badge('서버 일괄', 'accent')
          : h('span.muted.small', {}, '아래 DB 개별 설정')),
      ));
    }

    for (const c of dbs) {
      const inScope = isSuperadmin || isInScope(draft, c);
      const level = isSuperadmin ? 'migrate' : effectiveLevel(draft, c);
      // 이 DB가 물려받는 값. 선택기의 "상속" 항목에 그 값을 적어 두어야
      // 아무것도 고르지 않았을 때 무엇이 적용되는지 알 수 있다.
      const inheritedLevel = (c.serverId && draft.serverCapabilities.get(c.serverId))
        ?? draft.defaultLevel;
      const inheritLabel = levels.find((l) => l.value === inheritedLevel)?.label ?? inheritedLevel;

      const shown = isSuperadmin
        ? new Set(caps.map((x) => x.value))
        : new Set(effectiveCaps(draft, c));

      rows.push(h('tr.access-db-row', { class: inScope ? '' : 'row-muted' },
        showCheckbox ? h('td.col-check', {}, h('input', {
          type: 'checkbox',
          checked: draft.items.has(c.id),
          onchange: (e) => {
            if (e.target.checked) draft.items.add(c.id);
            else draft.items.delete(c.id);
            rebuild();
          },
        })) : null,
        h('td.access-db-name', {},
          h('div.cell-main', {}, c.name, envBadge(c.environment)),
          h('div.cell-sub', {}, c.databaseName || '—'),
        ),
        h('td', {}, levelPicker(draft.capabilities, c.id, inheritLabel, isSuperadmin || !inScope)),
        h('td', {},
          capCell(draft.capOverrides, c.id, shown, isSuperadmin || !inScope,
            '이 DB의 데이터 능력을 상속으로 되돌립니다'),
          draft.capOverrides.has(c.id) && !isSuperadmin ? badge('개별 설정', 'accent') : null),
        h('td', {}, inScope ? levelBadge(level) : badge('접근 불가', 'neutral')),
      ));
    }
  }

  return h('table.table.access-table', {},
    h('thead', {}, h('tr', {},
      showCheckbox ? h('th.col-check', {}, checkboxLabel) : null,
      h('th', {}, '서버 · DB'),
      h('th', {}, '능력 등급'),
      h('th', {}, '데이터 능력'),
      h('th', {}, '실효 권한'),
    )),
    h('tbody', {}, rows),
  );
}
