// 보안 설정 화면 (슈퍼 어드민 전용).
//
// 두 가지를 한 화면에 둔다: 2단계 인증 정책과 내부 시계 상태.
// 붙여 놓은 이유는 둘이 같은 사고에서 만난다 — "코드가 안 맞는다"는 문의가 오면
// 정책을 확인하기 전에 시계부터 봐야 하고, 그 두 숫자가 다른 화면에 있으면
// 원인을 찾는 동안 사용자는 계속 못 들어온다.
import { api } from '../core/api.js';
import { loadSession } from '../core/store.js';
import {
  h, mount, icon, spinner, pageHeader, toast, toastError, badge,
  confirmDialog, formatDate,
} from '../core/ui.js';
import { navigate } from '../core/router.js';

export async function renderSecurity(outlet) {
  mount(outlet, spinner());
  let data;
  try {
    data = await api.get('/security/');
  } catch (err) {
    mount(outlet, h('div.card.error-panel', {}, icon('alert', 24),
      h('div', {}, h('strong', {}, err.message ?? '보안 설정을 불러오지 못했습니다'))));
    return;
  }

  const reload = () => renderSecurity(outlet);
  mount(outlet,
    pageHeader('보안 설정', '2단계 인증 정책과 내부 시계 상태'),
    totpPolicyCard(data, reload),
    clockCard(data.clock, reload),
  );
}

function totpPolicyCard(data, reload) {
  const required = Boolean(data.policy?.totpRequired);
  const { enrolled = 0, missing = 0, totalUsers = 0 } = data.totp ?? {};

  const toggle = h('button', {
    type: 'button',
    class: required ? 'btn btn-danger' : 'btn btn-primary',
    onclick: () => setRequired(!required, missing, reload),
  }, required ? '의무화 해제' : '모든 사용자에게 의무화');

  return h('div.card', {},
    h('div.card-title', {},
      h('span', {}, '2단계 인증 정책'),
      required ? badge('의무', 'danger') : badge('자율', 'neutral'),
    ),
    h('p.field-help', {},
      required
        ? '모든 사용자가 2단계 인증을 설정해야 합니다. 설정하지 않은 사용자는 로그인 후 등록 화면에서 멈추며, ' +
          'API 토큰도 동작하지 않습니다. 본인이 직접 해제할 수 없습니다.'
        : '각 사용자가 프로필 화면에서 자율적으로 켭니다(기본값). 켜지 않은 사용자는 비밀번호만으로 로그인합니다.'),

    h('div.security-stat', {},
      statTile('설정 완료', enrolled, 'success'),
      statTile('미설정', missing, missing > 0 ? 'warn' : 'neutral'),
      statTile('전체 계정', totalUsers, 'neutral'),
    ),

    // 의무화하기 전에 미설정 인원을 눈에 띄게 알려 준다. 이 숫자를 모른 채 켜면
    // 그 인원이 다음 로그인에서 한꺼번에 등록 화면을 만난다.
    missing > 0 && !required
      ? h('p.notice.notice-warn', {}, icon('alert'),
        `아직 ${missing}명이 2단계 인증을 설정하지 않았습니다. 의무화하면 이들은 다음 로그인에서 등록을 마쳐야 합니다.`)
      : null,

    h('div.form-actions', {},
      h('button.btn', { type: 'button', onclick: () => navigate('/users') },
        icon('users'), '사용자별 현황 보기'),
      toggle,
    ),
    data.policy?.updatedAt
      ? h('p.muted.small', {}, `마지막 변경 ${formatDate(data.policy.updatedAt)}`)
      : null,
  );
}

function statTile(label, value, kind) {
  return h('div.security-tile', { class: `is-${kind}` },
    h('div.security-tile-value', {}, String(value)),
    h('div.security-tile-label', {}, label),
  );
}

async function setRequired(next, missing, reload) {
  if (next) {
    const ok = await confirmDialog({
      title: '2단계 인증 의무화',
      message: missing > 0
        ? `설정하지 않은 ${missing}명은 다음 로그인에서 등록을 마쳐야 다른 화면을 쓸 수 있습니다. 계속할까요?`
        : '모든 사용자에게 2단계 인증을 요구합니다. 계속할까요?',
      confirmLabel: '의무화',
    });
    if (!ok) return;
  }
  try {
    await api.put('/security/', { totpRequired: next });
    await loadSession();
    toast(next ? '2단계 인증을 의무화했습니다' : '의무화를 해제했습니다', 'success');
    reload();
  } catch (err) {
    toastError(err);
  }
}

// clockCard는 내부 시계 상태를 보여준다.
//
// 이 카드가 답하는 질문: "왜 인증 코드가 안 맞는가."
// 세 시각을 나란히 두면 어디가 틀렸는지 바로 보인다 —
// 내부 시각(우리가 믿는 것), 시스템 시각(OS가 말하는 것), 브라우저 시각(보는 사람의 것).
function clockCard(clock, reload) {
  if (!clock) return null;

  const browserSkew = Math.round((Date.now() - new Date(clock.internalTime).getTime()) / 1000);
  const rows = [
    ['DB Studio 내부 시각', formatTime(clock.internalTime), '2단계 인증 코드를 계산하는 데 쓰는 시각입니다'],
    ['서버 시스템 시각', formatTime(clock.systemTime), '운영체제가 알려주는 시각입니다'],
    ['학습된 보정값', signedSeconds(clock.offsetSeconds),
      `인증 앱들과 대조해 스스로 알아낸 오차입니다 (${clock.learned ?? 0}회 갱신)`],
    ['이 브라우저와의 차이', signedSeconds(browserSkew), '이 화면을 보고 있는 기기의 시각과 비교한 값입니다'],
  ];

  const big = Math.abs(clock.offsetSeconds ?? 0) >= 60 || Math.abs(browserSkew) >= 60;

  return h('div.card', {},
    h('div.card-title', {}, '내부 시계'),
    h('p.field-help', {},
      'DB Studio는 서버 시계를 그대로 믿지 않고, 인증 앱들과 대조해 스스로 시각을 맞춥니다. ' +
      '실행 중에 시스템 시각이 바뀌어도 내부 시각은 흔들리지 않습니다.'),
    h('dl.kv-grid', {}, rows.map(([k, v, help]) =>
      h('div.kv', {},
        h('dt', {}, k),
        h('dd', {}, h('div', {}, v), h('div.muted.small', {}, help)),
      ))),
    big
      ? h('p.notice.notice-warn', {}, icon('alert'),
        '시각 차이가 1분을 넘습니다. 인증 앱 코드가 자주 거부된다면 서버의 시간 동기화(NTP)를 확인하세요. ' +
        '그동안에도 DB Studio는 보정값으로 스스로 맞춰 동작합니다.')
      : h('p.muted.small', {}, '시각 차이가 크지 않습니다.'),
    h('div.form-actions', {},
      h('button.btn', { type: 'button', onclick: reload }, icon('refresh'), '새로고침'),
    ),
  );
}

function formatTime(value) {
  if (!value) return '—';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '—';
  const pad = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
    `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

// signedSeconds는 부호를 붙여 "앞선다/뒤진다"를 읽을 수 있게 한다.
// 초 단위 숫자만 보여 주면 방향을 알 수 없다.
function signedSeconds(secs) {
  const n = Number(secs) || 0;
  if (n === 0) return '차이 없음';
  const abs = Math.abs(n);
  const text = abs >= 60
    ? `${Math.floor(abs / 60)}분 ${abs % 60}초`
    : `${abs}초`;
  return n > 0 ? `+${text} (앞섬)` : `-${text} (뒤짐)`;
}
