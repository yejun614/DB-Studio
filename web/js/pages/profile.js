// 내 프로필 화면. 표시에 관한 것(이름·이메일·아이콘)만 여기서 바꾼다.
// 역할·상태·접근 권한은 슈퍼 어드민의 사용자 관리 화면에 있다 — 스스로 올릴 수 없어야 한다.
import { api } from '../core/api.js';
import { state, loadSession, loadMeta } from '../core/store.js';
import {
  h, mount, icon, field, input, select, spinner, pageHeader, toast, toastError,
  openModal, confirmDialog, copyToClipboard, roleBadge, badge,
  formatDate, relativeTime,
} from '../core/ui.js';
import { navigate } from '../core/router.js';
import {
  avatarNode, avatarIcon, hasAvatarIcon, initial, AVATAR_UPLOAD,
} from '../core/avatars.js';
import { openTOTPSetup, openTOTPDisable, openRecoveryRegenerate } from './totp.js';

export async function renderProfile(outlet) {
  mount(outlet, spinner());
  try {
    await loadMeta();
  } catch (err) {
    mount(outlet, h('div.card.empty', {}, icon('alert', 24),
      h('p', {}, '아이콘 목록을 불러오지 못했습니다'),
      h('p.muted', {}, err.message ?? String(err))));
    return;
  }

  const user = state.user;
  // 선택 상태는 저장 전까지 화면에만 있다. 아이콘을 눌러 보며 고르는 동작이므로
  // 클릭마다 저장하면 되돌리기가 어렵고 감사 로그가 시도 횟수만큼 쌓인다.
  //
  // 업로드는 예외다. 파일 자체를 폼과 함께 보내려면 multipart 전송과 나머지 필드의
  // JSON 전송을 한 요청에 섞어야 하는데, 그러면 양쪽 검증이 한 핸들러에 엉킨다.
  // 이미지는 고르는 즉시 별도 API로 저장되고(그 순간 avatar='upload'가 된다),
  // 이름·이메일·아이콘은 기존대로 저장 버튼에서 함께 보낸다.
  let picked = user?.avatar ?? '';

  const nameInput = input({ value: user?.displayName ?? '', maxlength: 60, required: true });
  const emailInput = input({ type: 'email', value: user?.email ?? '', maxlength: 254 });
  const preview = h('div.profile-preview');
  const saveBtn = h('button.btn.btn-primary', { type: 'submit' }, '저장');

  const drawPreview = () => {
    mount(preview,
      avatarNode({ ...user, avatar: picked, displayName: nameInput.value }, { size: 64 }),
      h('div', {},
        h('div.profile-preview-name', {}, nameInput.value.trim() || user?.username || ''),
        h('div.muted.small', {}, pickedLabel(picked)),
      ),
    );
  };

  const form = h('form.profile-form', {
    onsubmit: async (e) => {
      e.preventDefault();
      const name = nameInput.value.trim();
      if (!name) {
        toast('이름을 입력하세요', 'error');
        nameInput.focus();
        return;
      }
      saveBtn.disabled = true;
      try {
        await api.patch('/auth/profile', {
          displayName: name,
          email: emailInput.value.trim(),
          avatar: picked,
        });
        // 사이드바와 다른 화면이 같은 state.user를 보므로 세션을 다시 읽는다.
        await loadSession();
        toast('프로필을 저장했습니다', 'success');
      } catch (err) {
        toastError(err);
      } finally {
        saveBtn.disabled = false;
      }
    },
  },
    h('div.profile-head', {}, preview),
    h('div.form-grid', {},
      field('이름', nameInput, '화면에 표시되는 이름입니다'),
      field('이메일', emailInput, '연락용 표시 정보입니다. 로그인에는 쓰이지 않습니다'),
    ),
    // 업로드 후에는 화면을 다시 그린다. 미리보기만 갱신하면 아래 아이콘 목록의
    // 선택 표시가 옛 선택에 남아 있어 무엇이 적용된 상태인지 알 수 없다.
    imagePicker(() => renderProfile(outlet)),
    avatarPicker(picked, (key) => { picked = key; drawPreview(); }),
    h('div.form-actions', {}, saveBtn),
  );

  nameInput.addEventListener('input', drawPreview);
  drawPreview();

  mount(outlet,
    pageHeader('내 프로필', '이름과 프로필 아이콘을 바꿉니다'),
    h('div.card', {}, h('div.card-title', {}, '프로필'), form),
    accountCard(user),
    twoFactorCard(() => renderProfile(outlet)),
    tokenCard(),
  );
}

// twoFactorCard는 2단계 인증을 켜고 끄는 자리다.
//
// 프로필 화면에 두는 이유는 API 토큰과 같다: 이것은 **내 계정의 열쇠**이고,
// 관리자가 대신 켜 줄 수 없다(공유 비밀은 내 휴대폰에만 있어야 한다).
function twoFactorCard(reload) {
  const box = h('div');
  const card = h('div.card', {}, h('div.card-title', {}, '2단계 인증'), box);

  async function load() {
    mount(box, spinner('상태를 확인하는 중…'));
    let totp;
    try {
      ({ totp } = await api.get('/auth/totp'));
    } catch (err) {
      mount(box, h('p.notice.notice-danger', {}, icon('alert'), err.message));
      return;
    }

    if (!totp.enabled) {
      mount(box,
        h('p.field-help', {},
          '비밀번호만으로는 계정 하나가 새면 이 앱이 열어 주는 모든 DB가 함께 샙니다. ' +
          '인증 앱의 6자리 코드를 두 번째 자물쇠로 겁니다.'),
        totp.required
          ? h('p.notice.notice-warn', {}, icon('alert'),
            '관리자가 2단계 인증을 의무화했습니다. 설정하지 않으면 다른 화면을 쓸 수 없습니다.')
          : null,
        h('div.form-actions', {},
          h('button.btn.btn-primary', {
            type: 'button', onclick: () => openTOTPSetup(reload),
          }, icon('shield'), '2단계 인증 켜기'),
        ),
      );
      return;
    }

    const low = totp.recoveryRemaining <= 2;
    mount(box,
      h('div.totp-status', {},
        badge('사용 중', 'success'),
        h('span.muted.small', {},
          `${formatDate(totp.confirmedAt)} 설정` +
          (totp.lastUsedAt ? ` · 마지막 사용 ${relativeTime(totp.lastUsedAt)}` : '')),
      ),
      h('dl.kv-grid', {},
        h('div.kv', {},
          h('dt', {}, '복구 코드'),
          h('dd', {},
            `${totp.recoveryRemaining} / ${totp.recoveryTotal} 남음`,
            low ? badge('부족', 'warn') : null,
          )),
        // 이 계정에 적용 중인 시각 보정값. 평소에는 0이지만, 코드가 자주 거부되는
        // 사람에게는 이 숫자가 원인을 말해 준다("당신 휴대폰이 3분 빠릅니다").
        totp.skewSeconds
          ? h('div.kv', {},
            h('dt', {}, '시각 보정'),
            h('dd', {}, `${totp.skewSeconds > 0 ? '+' : ''}${totp.skewSeconds}초`,
              h('div.muted.small', {}, 'DB Studio가 이 계정의 인증 앱과 맞춰 둔 값입니다')))
          : null,
      ),
      low
        ? h('p.notice.notice-warn', {}, icon('alert'),
          '복구 코드가 얼마 남지 않았습니다. 휴대폰을 잃었을 때 쓸 수단이므로 미리 재발급해 두세요.')
        : null,
      h('div.form-actions', {},
        h('button.btn', {
          type: 'button', onclick: () => openRecoveryRegenerate(reload),
        }, icon('refresh'), '복구 코드 재발급'),
        totp.required
          ? h('button.btn', { type: 'button', disabled: true, title: '관리자가 의무화했습니다' },
            icon('lock'), '해제 불가')
          : h('button.btn.btn-danger-ghost', {
            type: 'button', onclick: () => openTOTPDisable(reload),
          }, '2단계 인증 끄기'),
      ),
      totp.required
        ? h('p.muted.small', {}, '관리자가 모든 사용자에게 2단계 인증을 의무화했습니다.')
        : null,
    );
  }

  load();
  return card;
}

// tokenCard는 API 토큰(MCP 클라이언트용 자격증명)을 관리한다.
//
// 프로필 화면에 두는 이유: 토큰은 **내 권한을 그대로 쓰는 내 것**이다. 남의 토큰을
// 만들 수 없고 관리자가 대신 만들어 줄 수도 없으므로, 사용자 관리 화면이 아니라
// 자기 계정 화면이 맞는 자리다.
function tokenCard() {
  const box = h('div');
  const card = h('div.card', {},
    h('div.card-title', {},
      h('span', {}, 'API 토큰'),
      h('button.btn.btn-small', {
        type: 'button', onclick: () => openTokenDialog(load),
      }, icon('plus'), '토큰 발급'),
    ),
    h('p.field-help', {},
      'Claude Code 같은 MCP 클라이언트, 그리고 REST API가 DB Studio에 붙을 때 쓰는 자격증명입니다. ' +
      '토큰은 발급한 사람의 권한을 그대로 씁니다 — 내가 볼 수 없는 DB는 토큰으로도 볼 수 없습니다.'),
    box,
  );

  async function load() {
    mount(box, spinner('토큰 목록을 불러오는 중…'));
    let res;
    try {
      res = await api.get('/auth/tokens');
    } catch (err) {
      mount(box, h('p.notice.notice-danger', {}, icon('alert'), err.message));
      return;
    }
    mount(box,
      res.tokens.length === 0
        ? h('p.muted.small', {}, '발급한 토큰이 없습니다.')
        : h('div.token-list', {}, res.tokens.map((t) => tokenRow(t, load))),
      mcpHint(res.mcpPath),
      restHint(res.apiPath),
    );
  }

  load();
  return card;
}

function tokenRow(t, reload) {
  const revoked = Boolean(t.revokedAt);
  const expired = t.expiresAt && new Date(t.expiresAt) < new Date();
  // 죽은 토큰(폐기·만료)은 흐리게 둔다. 지우지는 않는다 — "이런 토큰이 있었고
  // 언제까지 살아 있었다"가 사고를 되짚을 때 필요한 사실이다.
  return h('div.token-row', { class: revoked || expired ? 'is-dead' : '' },
    h('div.token-main', {},
      h('div.token-name', {},
        h('span.token-label', {}, t.name),
        badge(t.scope === 'write' ? '쓰기' : '읽기', t.scope === 'write' ? 'warn' : 'neutral'),
        revoked ? badge('폐기됨', 'neutral') : null,
        !revoked && expired ? badge('만료됨', 'neutral') : null,
      ),
      // 앞자리는 로그에서 이 토큰을 알아보는 유일한 단서다. 눈에 띄게 둔다.
      h('div.token-meta', {},
        h('code.token-prefix', {}, `${t.prefix}…`),
        // 재발급했다면 지금 살아 있는 값이 언제 나온 것인지를 앞에 둔다. "발급"만
        // 있으면 어제 재발급한 토큰이 석 달 전 것처럼 보이고, 그러면 클라이언트에
        // 넣어 둔 값이 최신인지 아무도 확신할 수 없다.
        h('span', {}, t.rotatedAt
          ? `재발급 ${formatDate(t.rotatedAt)}`
          : `발급 ${formatDate(t.createdAt)}`),
        h('span', {}, t.expiresAt ? `만료 ${formatDate(t.expiresAt)}` : '만료 없음'),
        t.rotatedAt ? h('span', {}, `처음 만든 날 ${formatDate(t.createdAt)}`) : null,
      ),
      h('div.token-meta', {},
        h('span', {}, t.lastUsedAt
          ? `마지막 사용 ${relativeTime(t.lastUsedAt)}${t.lastUsedIp ? ` · ${t.lastUsedIp}` : ''}`
          : '사용 기록 없음')),
    ),
    h('div.token-actions', {},
      // 값만 다시 발급한다. 값이 샜을 때 할 일은 대개 "이 토큰을 버리기"가 아니라
      // "이 토큰의 값 바꾸기"다 — 클라이언트 설정이 가리키는 것은 이름이고, 새로
      // 만들어 옮기면 고칠 곳이 늘어나며 그 사이 옛 토큰 지우는 것을 잊는다.
      //
      // 폐기 단추는 없앴다. 값을 바꾸거나(재발급) 아예 지우거나(삭제) 둘로 충분하고,
      // 셋을 늘어놓으면 "폐기와 삭제는 뭐가 다른가"를 매번 생각해야 한다.
      revoked ? null : h('button.btn.btn-small', {
        type: 'button',
        title: '이름·범위·만료는 그대로 두고 값만 새로 만듭니다',
        onclick: () => rotateToken(t, reload),
      }, icon('refresh'), '재발급'),
      h('button.icon-btn.danger', {
        type: 'button', title: '기록에서 지우기',
        onclick: async () => {
          const ok = await confirmDialog({
            title: '토큰 기록 삭제',
            message: `${t.name} 을(를) 목록에서 지웁니다. 이 토큰을 쓰는 클라이언트는 `
              + '즉시 접속할 수 없게 되고, 이런 토큰이 있었다는 기록도 남지 않습니다.',
            confirmLabel: '삭제', danger: true,
          });
          if (!ok) return;
          try {
            await api.del(`/auth/tokens/${t.id}`);
            reload();
          } catch (err) { toastError(err); }
        },
      }, icon('trash')),
    ),
  );
}

// rotateToken은 값만 새로 발급하고 그 값을 한 번 보여준다.
//
// 먼저 묻는 이유: 누르는 순간 지금 쓰이고 있는 값이 죽는다. 되돌릴 수 없고, 새 값을
// 넣기 전까지 그 클라이언트는 붙지 못한다.
async function rotateToken(t, reload) {
  const ok = await confirmDialog({
    title: '토큰 값 재발급',
    message: `${t.name} 의 값을 새로 만듭니다. 지금 쓰던 값은 그 즉시 못 쓰게 되므로, `
      + '새 값을 클라이언트에 넣기 전까지는 접속이 끊깁니다.',
    confirmLabel: '재발급',
    details: h('p.notice.notice-info', {}, icon('activity'),
      h('span', {},
        '이름·범위·만료는 그대로입니다', t.expiresAt ? ` (만료 ${formatDate(t.expiresAt)})` : '',
        '. 만료가 얼마 남지 않았다면 재발급해도 그대로입니다 — 그때는 새 토큰을 만드세요.')),
  });
  if (!ok) return;
  try {
    const res = await api.post(`/auth/tokens/${t.id}/rotate`);
    showTokenOnce(res.value, '토큰 값을 다시 발급했습니다');
    reload();
  } catch (err) { toastError(err); }
}

function mcpHint(mcpPath) {
  const url = `${location.origin}${mcpPath}`;
  return h('details.mcp-hint', {},
    h('summary', {}, 'MCP 클라이언트 연결 방법'),
    h('p.field-help', {}, 'Claude Code:'),
    h('pre.code-block', {},
      `claude mcp add --transport http dbstudio ${url} \\\n  --header "Authorization: Bearer <토큰>"`),
    h('p.field-help', {}, '설정 파일을 직접 쓰는 클라이언트:'),
    h('pre.code-block', {},
      JSON.stringify({
        mcpServers: {
          dbstudio: { type: 'http', url, headers: { Authorization: 'Bearer <토큰>' } },
        },
      }, null, 2)),
    h('p.field-help', {},
      '읽기 토큰은 조회 툴만 보입니다. 쓰기 토큰이라도 기존 안전장치' +
      '(마이그레이션 승인 수, 운영 DB 확인 문구, 능력 판정)는 그대로 적용됩니다.'),
  );
}

// restHint는 같은 토큰으로 부르는 REST API 사용법을 보여준다.
//
// MCP 안내와 나란히 두는 이유: 둘은 같은 토큰, 같은 툴, 같은 권한을 쓰는 두 개의 문이다.
// 스크립트나 CI에서 부를 때는 JSON-RPC 봉투가 필요 없고 curl 한 줄이면 된다.
function restHint(apiPath) {
  if (!apiPath) return null;
  const url = `${location.origin}${apiPath}`;
  return h('details.mcp-hint', {},
    h('summary', {}, 'REST API로 부르기 (스크립트·CI)'),
    h('p.field-help', {}, '쓸 수 있는 툴과 인자 스키마 확인:'),
    h('pre.code-block', {},
      `curl -H "Authorization: Bearer <토큰>" ${url}`),
    h('p.field-help', {}, '툴 실행 — 본문이 곧 인자입니다:'),
    h('pre.code-block', {},
      `curl -X POST ${url}/query_data \\\n` +
      '  -H "Authorization: Bearer <토큰>" \\\n' +
      '  -H "Content-Type: application/json" \\\n' +
      '  -d \'{"connection":"운영 DB","table":"users","limit":10}\''),
    h('p.field-help', {},
      '성공하면 200과 함께 결과가 JSON으로 옵니다. 툴이 거절하면 422, 권한이 없으면 403, ' +
      '토큰이 잘못되었으면 401입니다 — 상태 코드만 보고 분기할 수 있습니다.'),
  );
}

function openTokenDialog(reload) {
  const name = input({ placeholder: '예: 노트북의 Claude Code', autocomplete: 'off' });
  const scope = select([
    { value: 'read', label: '읽기 — 조회 툴만' },
    { value: 'write', label: '쓰기 — 조회 + 변경 툴' },
  ], { value: 'read' });
  const expiry = select([
    { value: '30', label: '30일' },
    { value: '90', label: '90일' },
    { value: '365', label: '1년' },
    { value: '0', label: '만료 없음' },
  ], { value: '90' });

  const warn = h('div');
  const syncWarn = () => {
    mount(warn, scope.value === 'write'
      ? h('p.notice.notice-warn', {}, icon('alert'),
        '쓰기 토큰을 든 프로그램은 사람이 화면에서 누르는 확인 단계를 거치지 않습니다. ' +
        '되돌릴 수 없는 동작(운영 DB 복구, 파괴적 마이그레이션 실행)은 여전히 웹 화면에서만 가능합니다.')
      : null);
  };
  scope.addEventListener('change', syncWarn);
  syncWarn();

  openModal({
    title: 'API 토큰 발급',
    width: 560,
    body: () => [
      field('이름', name, '어디에 쓰는 토큰인지 적어 두면 나중에 정리하기 쉽습니다'),
      field('범위', scope),
      field('만료', expiry),
      warn,
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          if (!name.value.trim()) {
            toast('토큰 이름을 입력하세요', 'error');
            return;
          }
          e.currentTarget.disabled = true;
          try {
            const res = await api.post('/auth/tokens', {
              name: name.value.trim(),
              scope: scope.value,
              expiresInDays: Number(expiry.value),
            });
            close();
            showTokenOnce(res.value);
            reload();
          } catch (err) {
            toastError(err);
            e.currentTarget.disabled = false;
          }
        },
      }, '발급'),
    ],
  });
}

// showTokenOnce는 발급된 토큰을 한 번만 보여준다.
// 서버가 해시만 저장하므로 이 창을 닫으면 되찾을 방법이 없다.
function showTokenOnce(value, title = '토큰이 발급되었습니다') {
  openModal({
    title,
    width: 620,
    body: () => [
      h('p.notice.notice-warn', {}, icon('alert'),
        h('span', {}, h('b', {}, '이 값은 다시 표시되지 않습니다. '),
          '지금 복사해 클라이언트 설정에 넣으세요. 잃어버리면 새로 발급해야 합니다.')),
      h('pre.code-block.token-value', {}, value),
      h('button.btn', { type: 'button', onclick: () => copyToClipboard(value) },
        icon('copy'), '복사'),
    ],
    footer: (close) => [h('button.btn.btn-primary', { type: 'button', onclick: close }, '닫기')],
  });
}

function pickedLabel(key) {
  if (!key) return '아이콘 없음 (이니셜 표시)';
  if (key === AVATAR_UPLOAD) return '직접 올린 이미지';
  const found = state.meta?.avatars?.find((a) => a.key === key);
  return found ? found.label : key;
}

// imagePicker는 파일 업로드와 URI 가져오기를 담당한다.
//
// 두 경로 모두 서버가 바이트를 받아 저장한다. URI를 그대로 두고 브라우저가 불러오게
// 하면 CSP를 외부 출처까지 열어야 하고, 링크가 깨지는 순간 아바타가 사라진다
// (자세한 이유는 internal/api/avatar_handlers.go).
function imagePicker(onUploaded) {
  const maxKB = state.meta?.avatarMaxKB ?? 512;
  const accept = (state.meta?.avatarMimes ?? []).join(',');
  const status = h('div.upload-status');
  const uriInput = input({ placeholder: 'https://example.com/me.png', autocomplete: 'off' });

  const fileInput = h('input.hidden-file', {
    type: 'file', accept,
    onchange: async (e) => {
      const file = e.target.files?.[0];
      if (!file) return;
      // 서버도 확인하지만 여기서 먼저 걸러 준다. 큰 파일을 올린 뒤에야
      // 거부당하면 업로드 시간을 통째로 버리게 된다.
      if (file.size > maxKB * 1024) {
        toast(`이미지는 ${maxKB}KB 이하여야 합니다 (고른 파일 ${Math.round(file.size / 1024)}KB)`, 'error');
        fileInput.value = '';
        return;
      }
      const form = new FormData();
      form.append('file', file);
      await send('/auth/avatar', { method: 'POST', body: form });
      fileInput.value = '';
    },
  });

  const send = async (path, options) => {
    mount(status, spinner('올리는 중…'));
    try {
      const res = await fetch(`/api/v1${path}`, {
        credentials: 'same-origin',
        headers: { 'X-Requested-With': 'dbstudio', ...(options.headers ?? {}) },
        ...options,
      });
      const payload = await res.json().catch(() => ({}));
      if (!res.ok) {
        throw new Error(payload.detail ? `${payload.message} (${payload.detail})` : payload.message);
      }
      await loadSession();
      mount(status);
      toast('프로필 이미지를 저장했습니다', 'success');
      onUploaded();
    } catch (err) {
      mount(status);
      toast(err.message || '이미지를 저장하지 못했습니다', 'error', 6000);
    }
  };

  const hasImage = state.user?.avatar === AVATAR_UPLOAD;

  return h('div.image-pick', {},
    h('div.field-label', {}, '프로필 이미지'),
    h('div.image-pick-row', {},
      h('button.btn.btn-small', { type: 'button', onclick: () => fileInput.click() },
        icon('copy'), '파일 선택'),
      fileInput,
      hasImage
        ? h('button.btn.btn-small.btn-danger-ghost', {
            type: 'button',
            onclick: async () => {
              await send('/auth/avatar', { method: 'DELETE' });
            },
          }, icon('trash'), '이미지 제거')
        : null,
    ),
    h('div.image-pick-row', {},
      uriInput,
      h('button.btn.btn-small', {
        type: 'button',
        onclick: async () => {
          const uri = uriInput.value.trim();
          if (!uri) {
            toast('이미지 주소를 입력하세요', 'error');
            return;
          }
          await send('/auth/avatar/uri', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ uri }),
          });
          uriInput.value = '';
        },
      }, '주소에서 가져오기'),
    ),
    status,
    h('span.field-help', {},
      `PNG·JPEG·GIF·WebP, 최대 ${maxKB}KB. 주소를 입력하면 서버가 한 번 내려받아 저장합니다.`),
  );
}

// avatarPicker는 서버가 준 목록을 그룹별로 그린다.
//
// 그릴 수 없는 키(서버 목록에는 있지만 web/js/core/avatars.js에 획이 없는 경우)는
// 빈 원으로 내보내지 않고 아예 제외한다. 고를 수는 있는데 아무것도 보이지 않는
// 항목은 고장으로 보인다.
function avatarPicker(current, onPick) {
  const groups = state.meta?.avatarGroups ?? [];
  const all = state.meta?.avatars ?? [];
  const wrap = h('div.avatar-pick');

  const options = [];
  const select = (key) => {
    for (const el of options) el.classList.toggle('is-on', el.dataset.avatar === key);
    onPick(key);
  };

  const optionButton = (key, label, content) => {
    const btn = h('button.avatar-option', {
      type: 'button', title: label, dataset: { avatar: key },
      'aria-label': label, onclick: () => select(key),
    }, content, h('span.avatar-option-label', {}, label));
    if (key === current) btn.classList.add('is-on');
    options.push(btn);
    return btn;
  };

  // "아이콘 없음"을 첫 항목으로 둔다. 되돌릴 자리가 목록 안에 있어야 한다.
  wrap.appendChild(h('div.avatar-group', {},
    h('div.avatar-group-title', {}, '기본'),
    h('div.avatar-options', {},
      optionButton('', '이니셜',
        h('span.avatar.avatar-sample', {}, initial(state.user))),
    ),
  ));

  for (const group of groups) {
    const items = all.filter((a) => a.group === group.key && hasAvatarIcon(a.key));
    if (items.length === 0) continue;
    wrap.appendChild(h('div.avatar-group', {},
      h('div.avatar-group-title', {}, group.label),
      h('div.avatar-options', {}, items.map((a) => optionButton(a.key, a.label,
        h('span.avatar.avatar-sample', {}, avatarIcon(a.key, 22))))),
    ));
  }

  return h('div.avatar-pick-wrap', {},
    h('div.field-label', {}, '프로필 아이콘'),
    wrap,
  );
}

// accountCard는 바꿀 수 없는 정보를 보여준다.
// 프로필 화면에 함께 두는 이유: "내 계정이 어떤 상태인가"를 확인하려고 들어오는
// 사람에게 관리자에게 물어야 할 것과 스스로 할 수 있는 것을 한 화면에서 구분해 준다.
function accountCard(user) {
  const rows = [
    ['아이디', user?.username ?? '—'],
    ['역할', roleBadge(user?.role)],
    // 전역 권한을 여기 보여준다. 이 사람이 매크로 메뉴를 왜 못 보는지 물어보기 전에
    // 스스로 확인할 수 있어야 한다.
    ['전역 권한', permList(user)],
    ['상태', user?.status === 'active' ? '활성' : '비활성'],
    ['마지막 로그인', lastLogin(user)],
    ['계정 생성', user?.createdAt ? formatDate(user.createdAt) : '—'],
  ];
  return h('div.card', {},
    h('div.card-title', {}, '계정'),
    // 각 쌍을 .kv로 감싼다. dt/dd를 그리드에 바로 넣으면 라벨과 값이
    // 서로 다른 열로 흩어진다(kv-grid는 쌍 하나를 한 칸으로 본다).
    h('dl.kv-grid', {}, rows.map(([k, v]) =>
      h('div.kv', {}, h('dt', {}, k), h('dd', {}, v)))),
    h('div.form-actions', {},
      h('button.btn', { type: 'button', onclick: () => navigate('/change-password') },
        icon('key'), '비밀번호 변경'),
    ),
    h('p.muted.small', {},
      '역할과 DB 접근 권한은 슈퍼 어드민이 설정합니다. 변경이 필요하면 관리자에게 요청하세요.'),
  );
}

function permList(user) {
  const defs = state.meta?.perms ?? [];
  const held = user?.role === 'superadmin' ? defs.map((p) => p.value) : (user?.perms ?? []);
  if (held.length === 0) return '없음';
  return h('span.perm-list', {}, held.map((value) =>
    badge(defs.find((p) => p.value === value)?.label ?? value, 'accent')));
}

function lastLogin(user) {
  if (!user?.lastLoginAt) return '기록 없음';
  const when = `${formatDate(user.lastLoginAt)} (${relativeTime(user.lastLoginAt)})`;
  return user.lastLoginIp ? `${when} · ${user.lastLoginIp}` : when;
}
