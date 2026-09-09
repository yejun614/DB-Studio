// DB 컨테이너 — 도커로 데이터베이스를 세우고 다루는 화면.
//
// ── 왜 첫 화면이 "상태"인가 ─────────────────────────────────────────
// 이 기능은 배치를 손봐야 켜지는 종류다. 소켓을 마운트했는가, 그 소켓을 열
// 권한이 있는가(이 앱은 nonroot 로 돈다), 데몬이 우리가 말하는 API 버전을
// 받는가. 셋 중 하나만 어긋나도 아무것도 되지 않는데, 그 사실이 "만들기가
// 실패했다"로만 나타나면 어디를 고쳐야 하는지 알 수 없다.
//
// 그래서 못 쓸 때 **무엇을 고쳐야 하는지**를 화면이 직접 말한다. 이 안내가
// 기능의 절반이다 — 서버에 붙어 로그를 뒤질 사람만 쓸 수 있는 기능이라면
// 화면에 둘 이유가 없다.
import { h, mount, icon } from '../core/dom.js';
import { api } from '../core/api.js';
import { pageHeader, spinner, badge, emptyState } from '../core/ui.js';
import { state } from '../core/store.js';
import { setScreenDetail } from '../core/screen.js';

export async function renderDBSetup(root) {
  mount(root, spinner('도커 상태를 확인하는 중…'));

  let data = null;
  let failure = null;
  try {
    data = await api.get('/docker/status');
  } catch (err) {
    failure = err;
  }
  draw(root, data, failure);
}

function draw(root, data, failure) {
  const st = data?.status ?? null;
  setScreenDetail([
    st?.reachable ? `도커 ${st.version} (${st.os}/${st.architecture})` : '도커에 닿지 못함',
  ]);

  mount(root,
    pageHeader('DB 컨테이너', '도커로 데이터베이스를 세우고 실행·중단합니다',
      h('button.btn.btn-small', {
        type: 'button', onclick: () => renderDBSetup(root),
      }, icon('refresh'), '다시 확인')),
    failure ? gateCard(failure) : statusCard(st, data?.apiVersion),
    st?.reachable && st.apiOk ? nextStepCard() : null,
  );
}

// gateCard는 이중 게이트에 막혔을 때다.
//
// 서버 스위치와 사용자 권한을 갈라서 말한다. 둘은 고치는 사람이 다르다 —
// 스위치는 프로세스를 띄우는 사람이, 권한은 관리자가 고친다. 뭉쳐서 "쓸 수
// 없습니다"라고 하면 누구에게 말해야 하는지 알 수 없다.
function gateCard(err) {
  const code = err?.code ?? err?.body?.error ?? '';
  if (code === 'docker_disabled') {
    return h('div.card', {},
      h('h2', {}, badge('꺼짐', 'warn'), ' 이 서버는 도커 기능이 꺼져 있습니다'),
      h('p.field-help', {},
        '프로세스를 띄우는 사람이 켜야 합니다. 권한 설정으로는 켤 수 없습니다 — '
        + '도커 소켓에 닿는다는 것은 그 기계에서 무엇이든 할 수 있다는 뜻이라, '
        + '화면의 클릭으로 켜질 일이 아닙니다.'),
      h('h3.erd-sub', {}, '켜는 방법'),
      h('pre.code-block', {},
        'docker compose 로 돌고 있다면 dbstudio 서비스에:\n'
        + '\n'
        + '    environment:\n'
        + '      DBSTUDIO_ALLOW_DOCKER: "true"\n'
        + '    volumes:\n'
        + '      - /var/run/docker.sock:/var/run/docker.sock\n'
        + '\n'
        + '바이너리로 돌고 있다면:\n'
        + '\n'
        + '    ./dbstudio -allow-docker'),
      h('p.field-help', {},
        '켠 뒤에도 사용자에게 ', h('strong', {}, 'DB 컨테이너 관리'),
        ' 권한을 따로 줘야 합니다(권한 화면).'),
      socketNote());
  }
  return h('div.card', {},
    h('h2', {}, badge('권한 없음', 'danger'), ' DB 컨테이너 관리 권한이 없습니다'),
    h('p.field-help', {},
      err?.message ?? '관리자에게 “DB 컨테이너 관리” 권한을 요청하세요.'));
}

// statusCard는 데몬 상태다.
function statusCard(st, ourAPI) {
  if (!st) return emptyState('상태를 읽지 못했습니다');

  if (!st.reachable) {
    return h('div.card', {},
      h('h2', {}, badge('닿지 못함', 'danger'), ' 도커에 닿지 못했습니다'),
      h('dl.kv', {},
        h('dt', {}, '소켓'), h('dd', {}, h('code', {}, st.socket)),
        h('dt', {}, '이유'), h('dd', {}, st.reason ?? '알 수 없음')),
      socketNote());
  }

  const rows = [
    ['도커', `${st.version} (API ${st.apiVersion})`],
    ['호스트', `${st.os} / ${st.architecture}`],
    ['컨테이너', `${st.containers}개 (도는 중 ${st.running}개)`],
    ['이미지', `${st.images}개`],
    ['CPU · 메모리', `${st.cpus}코어 · ${gib(st.memTotal)}`],
    ['스웜', swarmLabel(st.swarmState)],
    ['소켓', st.socket],
  ];

  return h('div.card', {},
    h('h2', {},
      st.apiOk ? badge('쓸 수 있음', 'success') : badge('버전 안 맞음', 'danger'),
      ' 도커에 닿았습니다'),
    !st.apiOk ? h('p.erd-panel-danger', {}, st.reason) : null,
    // API 버전이 맞을 때는 우리 쪽 버전을 굳이 보여주지 않는다. 맞는데도
    // 숫자가 둘 떠 있으면 무엇을 견주라는 것인지 사람이 되묻게 된다.
    !st.apiOk ? h('p.field-help', {}, `이 앱이 말하는 버전: ${ourAPI}`) : null,
    h('dl.kv', {}, ...rows.flatMap(([k, v]) => [h('dt', {}, k), h('dd', {}, String(v))])),
    st.reason && st.apiOk ? h('p.field-help', {}, st.reason) : null,
    (st.warnings ?? []).length
      ? h('div', {},
        h('h3.erd-sub', {}, '데몬이 남긴 경고'),
        h('ul.field-help', {}, ...st.warnings.map((w) => h('li', {}, w))))
      : null);
}

// nextStepCard는 다음 단계가 아직 없다는 것을 정직하게 적는다.
//
// 빈 화면을 두지 않는 이유: 쓸 수 있다고 말해 놓고 할 수 있는 일이 없으면,
// 사람은 자기가 무언가를 놓쳤다고 생각하고 화면을 다시 뒤진다.
function nextStepCard() {
  return h('div.card', {},
    h('h2', {}, '다음 단계'),
    h('p.field-help', {},
      '도커는 준비됐습니다. DB 종류를 고르고 설정을 정해 컨테이너를 만드는 화면은 '
      + '다음 단계에서 붙습니다 — 지금은 연결과 권한이 제대로 열렸는지만 확인합니다.'));
}

// socketNote는 소켓을 넣는 사람이 반드시 부딪히는 것을 미리 적는다.
//
// 이 앱은 nonroot(65532)로 돌고 도커 소켓은 보통 root:docker 다. 소켓을
// 마운트해도 열 수 없고, 그 오류는 "permission denied" 한 줄이다.
function socketNote() {
  return h('details.field-help', {},
    h('summary', {}, '소켓을 넣었는데도 권한 오류가 난다면'),
    h('p', {},
      '이 앱은 nonroot(uid 65532)로 돌고, 도커 소켓은 보통 root:docker 입니다. '
      + '마운트만 해서는 열 수 없습니다. 호스트의 docker 그룹 gid 를 확인해 '
      + '컨테이너에 그 그룹을 주세요.'),
    h('pre.code-block', {},
      'getent group docker      # 예: docker:x:988:\n'
      + '\n'
      + '    services:\n'
      + '      dbstudio:\n'
      + '        group_add: ["988"]'),
    h('p', {},
      '소켓을 그대로 주는 것이 부담스러우면 소켓 프록시를 앞에 두고 필요한 '
      + '경로만 여는 방법도 있습니다. 그때는 -docker-socket 으로 프록시의 '
      + '소켓을 가리키세요.'));
}

function swarmLabel(state_) {
  switch (state_) {
    case 'active': return '켜져 있음 (스웜 모드)';
    case 'pending': return '준비 중';
    case 'locked': return '잠김';
    case 'error': return '오류';
    default: return '꺼져 있음';
  }
}

function gib(bytes) {
  if (!bytes || bytes <= 0) return '알 수 없음';
  return `${(bytes / (1024 ** 3)).toFixed(1)} GiB`;
}

// 메뉴가 보이려면 서버 스위치와 권한이 둘 다 있어야 한다(auth_handlers.go).
// 그런데도 주소를 직접 치고 들어올 수 있으므로 화면이 다시 확인한다 —
// 실제 관문은 서버이고 이것은 안내다.
export function dockerMenuVisible() {
  return Boolean(state.permissions?.dockerManage);
}
