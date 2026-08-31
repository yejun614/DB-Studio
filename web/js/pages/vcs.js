// Git 연동: 저장소 설정 등록/확인, 푸시 이력.
import { api } from '../core/api.js';
import { withProject } from '../core/project.js';
import {
  h, mount, icon, select, input, checkbox, spinner, emptyState, pageHeader,
  badge, envBadge, toast, toastError, relativeTime, formatDate, openModal, confirmDialog,
} from '../core/ui.js';
import { errorPanel } from './users.js';
import { serverDbPicker } from '../core/connpick.js';

const PROVIDER_LABEL = { github: 'GitHub', gitlab: 'GitLab', bitbucket: 'Bitbucket' };

export async function renderVCS(outlet) {
  mount(outlet, spinner('Git 연동을 불러오는 중…'));

  let res;
  let conns;
  let pushes;
  try {
    [res, conns, pushes] = await Promise.all([
      api.get('/vcs/integrations'),
      api.get(withProject('/connections/')),
      api.get('/vcs/pushes?limit=30'),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  // Git 연동은 **내 계정**이다. 남의 것은 목록에 오지도 않고, 등록·수정·삭제에
  // 별도의 권한이 필요하지 않다 — 등록하는 사람과 쓰는 사람이 언제나 같기 때문이다.
  const reload = () => renderVCS(outlet);

  mount(outlet,
    pageHeader('Git 연동', '내 Git 계정으로 스키마 변경을 커밋하고 PR/MR을 만듭니다', [
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: () => openIntegrationDialog(null, res, conns, reload),
      }, icon('plus'), '연동 추가'),
    ]),
    h('p.notice.notice-info', {}, icon('alert'),
      '여기 등록한 Git 계정은 나만 볼 수 있고 나만 씁니다(슈퍼 어드민도 볼 수 없습니다). ' +
      '내가 올린 PR은 내 계정으로 열리고, 이 DB Studio 계정이 삭제되면 함께 지워집니다.'),
    res.items.length === 0
      ? emptyState('등록된 Git 연동이 없습니다. 저장소와 내 토큰을 등록하면 ' +
        '마이그레이션을 브랜치에 커밋하고 PR을 만들 수 있습니다.',
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: () => openIntegrationDialog(null, res, conns, reload),
      }, '연동 추가'))
      : h('div.vcs-list', {}, res.items.map((item) =>
        integrationCard(item, res, conns, reload))),
    pushHistory(pushes.pushes),
  );
}

function integrationCard(item, meta, conns, reload) {
  const conn = conns.items.find((i) => i.connection.id === item.connectionId);
  const checkBadge = item.lastCheckAt
    ? (item.lastCheckOk ? badge('확인됨', 'success') : badge('확인 실패', 'danger'))
    : badge('확인 안 됨', 'neutral');

  return h('article.card.vcs-card', {},
    h('div.vcs-card-head', {},
      h('div.vcs-card-title', {},
        h('strong', {}, item.name),
        badge(PROVIDER_LABEL[item.provider] ?? item.provider, 'accent'),
        item.enabled ? null : badge('비활성', 'neutral'),
        checkBadge,
      ),
      h('div.row-actions', {},
          h('button.btn.btn-small', {
            type: 'button',
            onclick: (e) => testIntegration(item, e.currentTarget, reload),
          }, icon('play'), '연결 확인'),
          h('button.icon-btn', {
            type: 'button', title: '수정',
            onclick: () => openIntegrationDialog(item, meta, conns, reload),
          }, icon('edit')),
          h('button.icon-btn', {
            type: 'button', title: '삭제',
            onclick: async () => {
              const ok = await confirmDialog({
                title: 'Git 연동 삭제',
                message: `"${item.name}" 연동을 삭제합니다. 저장된 토큰도 함께 지워집니다.`,
                confirmLabel: '삭제', danger: true,
              });
              if (!ok) return;
              try {
                await api.del(`/vcs/integrations/${encodeURIComponent(item.id)}`);
                toast('연동을 삭제했습니다', 'success');
                reload();
              } catch (err) {
                toastError(err);
              }
            },
          }, icon('trash')),
      ),
    ),
    h('dl.vcs-meta', {},
      metaRow('저장소', h('code', {}, item.repo)),
      metaRow('기준 브랜치', h('code', {}, item.defaultBranch)),
      metaRow('주소', item.baseUrl ? h('code', {}, item.baseUrl) : h('span.muted', {}, '공개 SaaS')),
      metaRow('브랜치 템플릿', h('code', {}, item.branchTemplate)),
      metaRow('경로 템플릿', h('code', {}, item.pathTemplate)),
      metaRow('토큰', item.hasToken ? badge('설정됨', 'success') : badge('없음', 'danger')),
      metaRow('적용 대상', conn
        ? h('span', {}, conn.connection.name, ' ', envBadge(conn.connection.environment))
        : h('span.muted', {}, '모든 커넥션')),
    ),
    item.lastCheckMsg
      ? h('p.vcs-check-msg', { class: item.lastCheckOk ? 'is-ok' : 'is-bad' },
        `${relativeTime(item.lastCheckAt)}: ${item.lastCheckMsg}`)
      : null,
  );
}

function metaRow(label, value) {
  return h('div.meta-row', {}, h('dt', {}, label), h('dd', {}, value));
}

async function testIntegration(item, btn, reload) {
  btn.disabled = true;
  const original = btn.textContent;
  btn.textContent = '확인 중…';
  try {
    const res = await api.post(`/vcs/integrations/${encodeURIComponent(item.id)}/test`);
    openModal({
      title: '연결 확인 결과',
      width: 560,
      body: () => [
        h('div.notice.notice-success', {}, icon('check'),
          h('div', {},
            h('strong', {}, '저장소에 접근할 수 있습니다'),
            h('p', {}, `${res.repo.fullName} · 기본 브랜치 ${res.repo.defaultBranch}`),
            res.repo.webUrl ? h('p.muted', {}, res.repo.webUrl) : null)),
        res.warnings?.length
          ? h('div.notice.notice-warn', {}, icon('alert'),
            h('ul.note-list', {}, res.warnings.map((w) => h('li', {}, w))))
          : null,
      ],
    });
    reload();
  } catch (err) {
    // 서버가 detail에 원인을 담아 보낸다. 토큰 만료나 저장소 이름 오타가 대부분이므로
    // 그 문장을 그대로 보여주는 것이 가장 빠른 단서다.
    openModal({
      title: '연결 확인 실패',
      width: 560,
      body: () => h('div.notice.notice-danger', {}, icon('alert'),
        h('div', {},
          h('strong', {}, err.message ?? '접근할 수 없습니다'),
          err.detail ? h('pre.sql-block', {}, err.detail) : null,
          h('ul.note-list', {},
            h('li', {}, '토큰이 만료되었거나 권한(저장소 쓰기)이 부족할 수 있습니다'),
            h('li', {}, '저장소 이름이 owner/repo 형식인지 확인하세요'),
            h('li', {}, 'self-hosted 인스턴스면 주소가 정확한지 확인하세요')))),
    });
    reload();
  } finally {
    btn.disabled = false;
    btn.textContent = original;
  }
}

function openIntegrationDialog(existing, meta, conns, reload) {
  const isEdit = Boolean(existing);
  const providers = meta.providers ?? [];

  const nameInput = input({ value: existing?.name ?? '', placeholder: '예: 스키마 저장소' });
  const providerSelect = select(
    providers.map((p) => ({ value: p.value, label: p.label })),
    { value: existing?.provider ?? 'github' },
  );
  const baseInput = input({ value: existing?.baseUrl ?? '' });
  const repoInput = input({ value: existing?.repo ?? '' });
  const branchInput = input({ value: existing?.defaultBranch ?? 'main' });
  const branchTmpl = input({ value: existing?.branchTemplate ?? 'schema/{date}-{slug}' });
  const pathTmpl = input({ value: existing?.pathTemplate ?? 'migrations/{ts}_{slug}' });
  const userInput = input({ value: existing?.username ?? '' });
  const tokenInput = input({
    type: 'password',
    placeholder: isEdit ? '(변경하지 않으려면 비워 두세요)' : '토큰 붙여넣기',
    autocomplete: 'new-password',
  });
  const enabledBox = checkbox('활성', { checked: existing ? existing.enabled : true });

  // 마이그레이션 등급이 있는 커넥션만 전용 연동 대상으로 제시한다.
  const usable = conns.items.filter((i) => i.accessible && i.level === 'migrate');
  const connSelect = serverDbPicker({
    usable,
    currentId: existing?.connectionId ?? '',
    onPick: () => {},
    serverLabel: '적용 대상',
    allLabel: '모든 커넥션에서 사용',
    serverHelp: '특정 커넥션 전용으로 지정하면 다른 커넥션의 마이그레이션에는 쓸 수 없습니다',
    inline: false,
  });

  const hints = h('div.vcs-hints');
  const refreshHints = () => {
    const p = providers.find((x) => x.value === providerSelect.value);
    mount(hints, p
      ? h('ul.note-list', {},
        h('li', {}, `저장소 형식: ${p.repoHint}`),
        h('li', {}, `토큰: ${p.tokenHint}`),
        h('li', {}, `주소: ${p.baseHint}`))
      : null);
    // Bitbucket 앱 비밀번호는 사용자 이름이 필요하다.
    userRow.style.display = providerSelect.value === 'bitbucket' ? '' : 'none';
  };
  const userRow = h('label.field', {},
    h('span.field-label', {}, '사용자 이름 (Bitbucket 앱 비밀번호용)'), userInput,
    h('span.field-help', {}, '액세스 토큰을 쓰면 비워 두세요'));
  providerSelect.addEventListener('change', refreshHints);

  const varsHelp = h('details.vcs-vars', {},
    h('summary', {}, '템플릿 변수'),
    h('ul.note-list', {}, (meta.templateVars ?? []).map((v) =>
      h('li', {}, h('code', {}, v.name), ` — ${v.help}`))));

  openModal({
    title: isEdit ? 'Git 연동 수정' : 'Git 연동 추가',
    width: 620,
    body: () => {
      setTimeout(refreshHints, 0);
      return [
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
        h('label.field', {}, h('span.field-label', {}, '서비스'), providerSelect),
        hints,
        h('label.field', {}, h('span.field-label', {}, '저장소'), repoInput),
        h('label.field', {}, h('span.field-label', {}, '주소 (self-hosted)'), baseInput,
          h('span.field-help', {},
            'http는 사설망 주소에만 허용됩니다 — 토큰이 평문으로 전송되기 때문입니다')),
        h('label.field', {}, h('span.field-label', {}, '기준 브랜치'), branchInput),
        userRow,
        h('label.field', {}, h('span.field-label', {}, '토큰'), tokenInput,
          h('span.field-help', {},
            '암호화되어 저장되며 화면이나 API 응답에 다시 표시되지 않습니다')),
        h('div.form-grid', {},
          h('label.field', {}, h('span.field-label', {}, '브랜치 템플릿'), branchTmpl),
          h('label.field', {}, h('span.field-label', {}, '경로 템플릿'), pathTmpl)),
        varsHelp,
        ...connSelect.nodes,
        enabledBox,
      ];
    },
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          const btn = e.currentTarget;
          btn.disabled = true;
          const payload = {
            name: nameInput.value,
            provider: providerSelect.value,
            baseUrl: baseInput.value,
            repo: repoInput.value,
            defaultBranch: branchInput.value,
            branchTemplate: branchTmpl.value,
            pathTemplate: pathTmpl.value,
            username: userInput.value,
            connectionId: connSelect.value,
            enabled: enabledBox.querySelector('input').checked,
          };
          // 수정 시 빈 토큰은 "유지"를 뜻하므로 아예 보내지 않는다.
          if (tokenInput.value.trim() !== '') payload.token = tokenInput.value;
          try {
            if (isEdit) {
              await api.put(`/vcs/integrations/${encodeURIComponent(existing.id)}`, payload);
            } else {
              await api.post('/vcs/integrations', payload);
            }
            close();
            toast(isEdit ? '연동을 수정했습니다' : '연동을 추가했습니다', 'success');
            reload();
          } catch (err) {
            btn.disabled = false;
            toastError(err);
          }
        },
      }, isEdit ? '저장' : '추가'),
    ],
  });
}

function pushHistory(pushes) {
  return h('section.card', {},
    h('h2', {}, '푸시 이력'),
    !pushes || pushes.length === 0
      ? h('p.muted', {}, '아직 푸시한 기록이 없습니다. 마이그레이션 화면에서 "Git에 올리기"를 사용하세요.')
      : h('div.table-wrap', {},
        h('table.table', {},
          h('thead', {}, h('tr', {},
            h('th', {}, '시각'), h('th', {}, '연동'), h('th', {}, '마이그레이션'),
            h('th', {}, '브랜치'), h('th', {}, '결과'))),
          h('tbody', {}, pushes.map((p) => h('tr', { class: p.status === 'ok' ? '' : 'is-destructive' },
            h('td.nowrap', {},
              h('div', {}, formatDate(p.createdAt)),
              p.actorName ? h('div.cell-sub', {}, p.actorName) : null),
            h('td', {},
              h('div', {}, p.integrationName || '—'),
              h('div.cell-sub', {}, PROVIDER_LABEL[p.provider] ?? p.provider)),
            h('td', {},
              p.migrationId
                ? h('a', { href: `/migrations/${encodeURIComponent(p.migrationId)}` }, p.migrationTitle || p.migrationId)
                : h('span.muted', {}, p.migrationTitle || '—')),
            h('td', {},
              h('code', {}, p.branch),
              p.branchCreated ? h('div.cell-sub', {}, '새로 만듦') : null,
              p.files?.length ? h('div.cell-sub', {}, `${p.files.length}개 파일`) : null),
            h('td', {},
              p.status === 'ok' ? badge('성공', 'success') : badge('실패', 'danger'),
              p.commitSha
                ? h('div.cell-sub', {}, p.commitUrl
                  ? h('a', { href: p.commitUrl, target: '_blank', rel: 'noopener noreferrer' },
                    p.commitSha.slice(0, 8))
                  : p.commitSha.slice(0, 8))
                : null,
              p.prUrl
                ? h('div.cell-sub', {},
                  h('a', { href: p.prUrl, target: '_blank', rel: 'noopener noreferrer' },
                    `PR #${p.prNumber ?? ''}`),
                  p.prExisting ? ' (기존)' : '')
                : null,
              p.error ? h('div.cell-sub.text-danger', {}, p.error) : null),
          ))))),
  );
}

// ---------- 마이그레이션 화면에서 쓰는 푸시 대화상자 ----------

// openPushDialog는 마이그레이션을 Git에 올리는 대화상자를 띄운다.
// migrations.js가 호출한다.
export async function openPushDialog(mig, conn, reload) {
  const box = h('div', {}, spinner('연동 목록을 불러오는 중…'));
  openModal({
    title: 'Git에 올리기',
    width: 620,
    body: () => box,
  });

  let res;
  try {
    res = await api.get(`/vcs/integrations?connection=${encodeURIComponent(conn.id)}`);
  } catch (err) {
    mount(box, errorPanel(err));
    return;
  }
  const usable = res.items.filter((i) => i.enabled && i.hasToken);
  if (usable.length === 0) {
    mount(box,
      h('div.notice.notice-info', {}, icon('alert'),
        h('div', {},
          h('strong', {}, '사용할 수 있는 Git 연동이 없습니다'),
          h('p', {}, '먼저 Git 연동을 등록하세요.'),
          h('a.btn.btn-small', { href: '/vcs' }, 'Git 연동 설정으로'))));
    return;
  }

  const intSelect = select(usable.map((i) => ({
    value: i.id,
    label: `${i.name} — ${PROVIDER_LABEL[i.provider] ?? i.provider} · ${i.repo}`,
  })), { value: usable[0].id });
  const branchInput = input({ placeholder: '(비우면 템플릿을 사용합니다)' });
  const baseInput = input({ value: usable[0].defaultBranch });
  const titleInput = input({ value: `[${conn.name}] ${mig.title}` });
  const prBox = checkbox('PR/MR 함께 만들기', { checked: true });
  const result = h('div');

  const syncDefaults = () => {
    const chosen = usable.find((i) => i.id === intSelect.value);
    if (chosen) baseInput.value = chosen.defaultBranch;
  };
  intSelect.addEventListener('change', syncDefaults);

  mount(box,
    h('p.modal-message', {},
      `"${mig.title}" 의 up/down SQL과 목표 스키마(JSON)를 한 커밋으로 올립니다.`),
    h('label.field', {}, h('span.field-label', {}, '연동'), intSelect),
    h('label.field', {}, h('span.field-label', {}, '브랜치'), branchInput,
      h('span.field-help', {}, '같은 이름의 브랜치가 이미 있으면 그 위에 커밋합니다')),
    h('label.field', {}, h('span.field-label', {}, '대상(기준) 브랜치'), baseInput),
    h('label.field', {}, h('span.field-label', {}, 'PR 제목'), titleInput),
    prBox,
    mig.destructiveCount > 0
      ? h('p.notice.notice-warn', {}, icon('alert'),
        `파괴적 변경 ${mig.destructiveCount}건이 PR 설명에 경고로 표시됩니다.`)
      : null,
    result,
    h('div.modal-foot-inline', {},
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          const btn = e.currentTarget;
          btn.disabled = true;
          mount(result, spinner('저장소에 올리는 중…'));
          try {
            const out = await api.post(`/migrations/${encodeURIComponent(mig.id)}/push`, {
              integrationId: intSelect.value,
              branch: branchInput.value,
              baseBranch: baseInput.value,
              title: titleInput.value,
              openPr: prBox.querySelector('input').checked,
            });
            mount(result, pushResultView(out));
            reload?.();
          } catch (err) {
            btn.disabled = false;
            // 실패 단계는 서버가 payload에 담아 보낸다 (message로는 표현할 수 없다).
            const stage = err.payload?.stage;
            mount(result, h('div.notice.notice-danger', {}, icon('alert'),
              h('div', {},
                h('strong', {}, err.message ?? '올리지 못했습니다'),
                stage ? h('p', {}, `실패 단계: ${stage}`) : null,
                err.detail ? h('pre.sql-block', {}, err.detail) : null)));
          }
        },
      }, icon('play'), '올리기'),
    ),
  );
}

function pushResultView(out) {
  const push = out.push ?? {};
  const pr = out.pullRequest;
  return h('div.notice.notice-success', {}, icon('check'),
    h('div', {},
      h('strong', {}, '저장소에 올렸습니다'),
      h('ul.note-list', {},
        h('li', {}, `브랜치 ${push.branch}${push.branchCreated ? ' (새로 만듦)' : ''}`),
        push.commitSha
          ? h('li', {}, '커밋 ', push.commitUrl
            ? h('a', { href: push.commitUrl, target: '_blank', rel: 'noopener noreferrer' },
              push.commitSha.slice(0, 8))
            : push.commitSha.slice(0, 8))
          : null,
        h('li', {}, `${push.files?.length ?? 0}개 파일`),
        pr
          ? h('li', {}, pr.existing ? '기존 PR/MR 사용: ' : 'PR/MR 생성: ',
            h('a', { href: pr.webUrl, target: '_blank', rel: 'noopener noreferrer' },
              `#${pr.number}`))
          : h('li', {}, 'PR/MR은 만들지 않았습니다'),
      )));
}
