// 앱 엔트리포인트: 세션 확인 → 셸 구성 → 라우팅.
import {
  api, setUnauthorizedHandler, setPasswordChangeHandler, setTOTPSetupHandler,
} from './core/api.js';
import { state, loadSession, loadMeta, clearSession, subscribe } from './core/store.js';
import { h, mount, icon, toast, toastError } from './core/ui.js';
import * as router from './core/router.js';
import * as theme from './core/theme.js';
import { avatarNode } from './core/avatars.js';
import * as sidebar from './core/sidebar.js';
import { renderLogin, renderPasswordChange, renderChangePasswordPage } from './pages/login.js';
import { renderDashboard } from './pages/dashboard.js';
import { renderUsers } from './pages/users.js';
import { renderProfile } from './pages/profile.js';
import { renderAccess } from './pages/access.js';
import { renderConnections } from './pages/connections.js';
import { renderSchema } from './pages/schema.js';
import { renderMonitor, renderEvents } from './pages/monitor.js';
import { renderRules } from './pages/rules.js';
import { renderHost } from './pages/host.js';
import { renderLogs } from './pages/logs.js';
import { renderERDList } from './pages/erd.js';
import { renderStructure } from './pages/structure.js';
import { renderERDEditor } from './pages/erdeditor.js';
import { renderMigrations, renderMigrationDetail, renderVersions } from './pages/migrations.js';
import { renderVCS } from './pages/vcs.js';
import { renderNoSQL } from './pages/nosql.js';
import { renderData } from './pages/data.js';
import { renderSQLConsole } from './pages/sqlconsole.js';
import { renderBackups } from './pages/backups.js';
import { renderMacros, renderMacroRuns } from './pages/macros.js';
import { renderMacroEditor } from './pages/macroeditor.js';
import { renderTriggers } from './pages/triggers.js';
import { renderAssistant, openAssistantPopup } from './pages/assistant.js';
import { renderManual } from './pages/manual.js';
import { renderAudit } from './pages/audit.js';
import { renderSecurity } from './pages/security.js';
import { renderNotify } from './pages/notify.js';
import { renderCluster } from './pages/cluster.js';
import { renderStorage } from './pages/storage.js';
import { renderBroker } from './pages/broker.js';
import { renderTOTPSetupPage } from './pages/totp.js';

const appRoot = document.getElementById('app');

// ---------- 라우트 정의 ----------

router.define('/', renderDashboard);
router.define('/connections', renderConnections);
router.define('/schema', renderSchema);
router.define('/nosql', renderNoSQL);
router.define('/data', renderData);
router.define('/sql', renderSQLConsole);
router.define('/backups', renderBackups);
router.define('/macros', renderMacros);
router.define('/macros/runs', renderMacroRuns);
router.define('/macros/triggers', renderTriggers);
router.define('/macros/:id', renderMacroEditor);
router.define('/monitor', renderMonitor);
router.define('/monitor/rules', renderRules);
router.define('/monitor/host', renderHost);
router.define('/events', renderEvents);
router.define('/logs', renderLogs);
router.define('/structure', renderStructure);
router.define('/erd', renderERDList);
router.define('/erd/:id', renderERDEditor);
router.define('/migrations', renderMigrations);
router.define('/migrations/:id', renderMigrationDetail);
router.define('/versions', renderVersions);
router.define('/vcs', renderVCS);
router.define('/assistant', renderAssistant);
router.define('/manual', renderManual);
router.define('/profile', renderProfile);
router.define('/change-password', renderChangePasswordPage);
router.define('/users', renderUsers);
router.define('/users/:id/access', renderAccess);
router.define('/audit', renderAudit);
router.define('/security', renderSecurity);
router.define('/notify', renderNotify);
router.define('/cluster', renderCluster);
router.define('/storage', renderStorage);
router.define('/broker', renderBroker);
router.setNotFound((outlet) => {
  mount(outlet, h('div.card.empty', {},
    icon('alert', 28),
    h('h2', {}, '페이지를 찾을 수 없습니다'),
    h('a.btn', { href: '/' }, '대시보드로'),
  ));
});

// ---------- 셸 ----------

// NAV는 메뉴를 묶음으로 나눈다.
//
// 스무 개가 넘는 항목을 한 줄로 늘어놓으면 "지금 하려는 일이 어디쯤인지"를 매번
// 처음부터 훑어야 한다. 묶음은 **일의 단계**를 따른다: 무엇이 있는지 보고(개요),
// 지금 상태를 살피고(관측), 값을 다루고(데이터), 바꿀 것을 설계해 반영하고(설계·변경),
// 반복되는 일을 맡기고(운영), 계정과 기록을 관리한다(관리).
//
// 화면 이름이 아니라 목적으로 묶은 것이 요점이다. "스키마"와 "데이터"는 둘 다 DB를
// 보는 화면이지만 하나는 구조를, 하나는 값을 본다 — 찾는 사람의 머릿속에서는 그 둘이
// 다른 서랍에 있다.
const NAV = [
  {
    section: '개요',
    items: [
      { path: '/', label: '대시보드', icon: 'shield' },
      { path: '/connections', label: 'DB 커넥션', icon: 'database' },
    ],
  },
  {
    section: '관측',
    items: [
      { path: '/monitor', label: '모니터링', icon: 'activity' },
      // 서버 컴퓨터는 커넥션 관리자만 본다: 호스트 이름·디스크 경로·시스템 로그는
      // 특정 DB를 볼 수 있는 사람이 아니라 서버를 운영하는 사람의 정보다.
      { path: '/monitor/host', label: '서버 컴퓨터', icon: 'monitor', requires: 'manageConnections' },
      { path: '/events', label: '이벤트', icon: 'alert' },
      { path: '/logs', label: '로그 / 통계', icon: 'list' },
    ],
  },
  {
    section: '데이터',
    items: [
      { path: '/schema', label: '스키마', icon: 'database' },
      { path: '/data', label: '데이터', icon: 'table', requires: 'anyData' },
      { path: '/sql', label: 'SQL 콘솔', icon: 'terminal', requires: 'anyData' },
      { path: '/nosql', label: 'Mongo·Redis', icon: 'list' },
      // 스토리지는 DB가 아니지만 "데이터가 어디에 있는가"라는 같은 질문의 다른 층이다.
      { path: '/storage', label: '스토리지', icon: 'save' },
      // 메시지 브로커도 마찬가지다. "데이터가 어디로 흐르는가"의 다른 층이다.
      { path: '/broker', label: '메시지 브로커', icon: 'activity' },
    ],
  },
  {
    section: '설계 · 변경',
    items: [
      { path: '/structure', label: '구조', icon: 'workflow' },
      { path: '/erd', label: 'ERD 설계', icon: 'edit' },
      { path: '/migrations', label: '마이그레이션', icon: 'play' },
      { path: '/versions', label: '버전 이력', icon: 'refresh' },
    ],
  },
  {
    section: '운영',
    items: [
      { path: '/macros', label: '매크로', icon: 'workflow', requires: 'macro' },
      { path: '/backups', label: '백업', icon: 'save' },
      { path: '/vcs', label: 'Git 연동', icon: 'copy' },
    ],
  },
  {
    section: '관리',
    items: [
      { path: '/users', label: '사용자', icon: 'users', requires: 'manageUsers' },
      { path: '/security', label: '보안 설정', icon: 'lock', requires: 'manageUsers' },
      // 알림 설정은 "누가 무엇을 알게 되는가"를 정하는 일이라 관리 묶음에 둔다.
      { path: '/notify', label: '알림', icon: 'bell', requires: 'manageUsers' },
      // 클러스터는 인프라 구성이다. 노드 주소와 복제 지연은 운영자만 볼 일이고,
      // 노드를 내리는 것은 그 노드가 담당하던 일이 멈춘다는 뜻이다.
      { path: '/cluster', label: '클러스터', icon: 'copy', requires: 'manageUsers' },
      { path: '/audit', label: '감사 로그', icon: 'list', requires: 'manageUsers' },
    ],
  },
  {
    // 도구는 맨 아래다. 다른 화면을 보다가 필요할 때 부르는 것이지,
    // 여기서 일을 시작하는 자리가 아니다.
    section: '도구',
    items: [
      // 어시스턴트는 화면을 옮기지 않고 팝업으로 뜬다. 스키마를 보다가, 데이터를
      // 보다가 물어보는 도구이므로 보고 있던 화면이 남아야 한다.
      // 페이지(/assistant)도 그대로 두었다 — 긴 조사에는 넓은 화면이 낫고,
      // 주소로 공유할 수도 있어야 한다. 팝업 머리의 버튼으로 옮겨 간다.
      { path: '/assistant', label: 'AI 어시스턴트', icon: 'settings', popup: true },
      { path: '/manual', label: '메뉴얼', icon: 'list' },
    ],
  },
];

function buildShell() {
  const outlet = h('main.content');
  const nav = h('nav.nav');

  for (const group of NAV) {
    const items = group.items.filter(
      (item) => !item.requires || state.permissions?.[item.requires]);
    // 권한이 없어 항목이 하나도 남지 않은 묶음은 라벨도 그리지 않는다.
    // 빈 제목만 남으면 "여기 뭔가 있었는데 안 보인다"로 읽힌다.
    if (items.length === 0) continue;

    nav.appendChild(h('div.nav-section', {}, group.section));
    for (const item of items) {
      if (item.popup) {
        nav.appendChild(h('button.nav-item.nav-tool', {
          type: 'button',
          dataset: { path: item.path },
          onclick: () => openAssistantPopup(),
        }, icon(item.icon, 18), h('span', {}, item.label)));
        continue;
      }
      nav.appendChild(h('a.nav-item', {
        href: item.path,
        dataset: { path: item.path },
      }, icon(item.icon, 18), h('span', {}, item.label)));
    }
  }

  const shell = h('div.shell', {},
    // 좁은 화면 전용 상단 막대. 넓은 화면에서는 CSS가 숨긴다.
    sidebar.createTopbar(),
    h('aside#sidebar.sidebar', {},
      h('a.brand', { href: '/' }, icon('database', 22), h('span', {}, 'DB Studio')),
      nav,
      h('div.sidebar-foot', {},
        // 칩 자체가 프로필로 가는 입구다. 프로필을 메뉴에 따로 두면 항목 하나를
        // 더 읽어야 하고, 사람들은 자기 이름을 먼저 누른다.
        userChip(),
        h('div.sidebar-actions', {},
          themeToggle(),
          h('button.btn.btn-small.btn-block', { type: 'button', onclick: logout },
            icon('logout'), '로그아웃'),
        ),
      ),
      sidebar.createResizer(),
    ),
    sidebar.createBackdrop(),
    outlet,
  );

  mount(appRoot, shell);
  appRoot.classList.remove('app-loading');
  sidebar.bindGlobalHandlers();
  router.setOutlet(outlet);
  return outlet;
}

// userChip은 사이드바 하단의 사용자 표시이자 프로필 화면 입구다.
//
// 셸은 한 번만 만들지만 이 칩의 내용(이름·아이콘)은 프로필 화면에서 바뀐다.
// 그래서 칩만 상태를 구독해 스스로 다시 그린다 — 저장했는데 사이드바가 옛 이름을
// 계속 보여주면 저장이 안 된 것으로 보인다.
let unsubscribeChip = null;

function userChip() {
  // 셸을 다시 만들 때(로그아웃 후 재로그인) 이전 구독을 버린다.
  // 버리지 않으면 사라진 칩을 그리는 리스너가 로그인마다 하나씩 쌓인다.
  unsubscribeChip?.();
  const chip = h('a.user-chip', { href: '/profile', title: '내 프로필' });
  const render = () => {
    mount(chip,
      avatarNode(state.user, { size: 32 }),
      h('div.user-meta', {},
        h('div.user-name', {}, state.user?.displayName || state.user?.username),
        h('div.user-role', {}, roleLabel(state.user?.role)),
      ),
    );
  };
  render();
  unsubscribeChip = subscribe(render);
  return chip;
}

// themeToggle은 시스템 → 라이트 → 다크를 순환하는 버튼이다.
// 라벨에 현재 상태를 적어 아이콘만으로 짐작하지 않게 한다.
function themeToggle() {
  const btn = h('button.btn.btn-small.icon-only', { type: 'button' });
  const render = () => {
    const mode = theme.currentMode();
    btn.title = `${theme.modeLabel(mode)} (눌러서 변경)`;
    btn.setAttribute('aria-label', theme.modeLabel(mode));
    mount(btn, icon(theme.modeIcon(mode), 15));
  };
  btn.addEventListener('click', () => {
    theme.applyMode(theme.nextMode(theme.currentMode()));
    render();
  });
  render();
  return btn;
}

function roleLabel(role) {
  return state.meta?.roles?.find((r) => r.value === role)?.label ?? role ?? '';
}

// highlightNav는 현재 경로에 해당하는 메뉴를 **하나만** 강조한다.
//
// 접두사가 겹치는 메뉴가 있다("모니터링" /monitor 과 "서버 컴퓨터" /monitor/host,
// "매크로" /macros 와 "실행 기록" /macros/runs). 접두사로만 판단하면 둘이 함께
// 강조되어 "지금 어디에 있는가"에 답이 두 개가 된다. 가장 길게 맞는 항목 하나만 켠다.
//
// 경계까지 보는 이유: 문자열 접두사만 보면 /macros 가 /macrosomething 에도 맞는다.
function highlightNav() {
  const path = location.pathname;
  const items = [...document.querySelectorAll('.nav-item')];

  let best = null;
  for (const el of items) {
    const target = el.dataset.path;
    if (!target) continue;
    const matches = target === '/'
      ? path === '/'
      : path === target || path.startsWith(`${target}/`);
    if (!matches) continue;
    if (!best || target.length > best.dataset.path.length) best = el;
  }

  for (const el of items) el.classList.toggle('active', el === best);
}

async function logout() {
  try {
    await api.post('/auth/logout');
  } catch {
    // 서버 오류와 무관하게 클라이언트 상태는 초기화한다.
  }
  clearSession();
  showLogin();
}

// ---------- 화면 전환 ----------

function showLogin() {
  appRoot.classList.remove('app-loading');
  router.setOutlet(null);
  renderLogin(appRoot, { onSuccess: enterApp });
}

function showPasswordChange() {
  appRoot.classList.remove('app-loading');
  router.setOutlet(null);
  renderPasswordChange(appRoot, { onSuccess: enterApp });
}

// showTOTPSetup은 2단계 인증 의무화 상태에서 등록을 마칠 때까지 다른 화면을 막는다.
// 비밀번호 강제 변경과 같은 자리, 같은 처리다.
function showTOTPSetup() {
  appRoot.classList.remove('app-loading');
  router.setOutlet(null);
  renderTOTPSetupPage(appRoot, {
    onSuccess: enterApp,
    username: state.user?.username,
  });
}

async function enterApp() {
  if (!state.user) {
    showLogin();
    return;
  }
  if (state.user.mustChangePassword) {
    showPasswordChange();
    return;
  }
  // 순서가 중요하다: 비밀번호를 먼저 바꾸게 한 뒤 2단계 인증을 등록시킨다.
  // 임시 비밀번호를 쓰는 계정에 두 번째 요소를 붙여 봐야, 그 임시 비밀번호를
  // 아는 사람이 여전히 첫 번째 요소를 쥐고 있다.
  if (state.permissions?.totpRequired && !state.user.totpEnabled) {
    showTOTPSetup();
    return;
  }
  try {
    await loadMeta();
  } catch (err) {
    toastError(err);
    return;
  }
  buildShell();
  await router.render();
  highlightNav();
}

// ---------- 부팅 ----------

setUnauthorizedHandler(() => {
  clearSession();
  showLogin();
});
setPasswordChangeHandler(() => {
  if (state.user) state.user.mustChangePassword = true;
  showPasswordChange();
});
// 관리자가 세션 도중에 의무화를 켰을 수도 있다. 그때는 다음 API 호출이
// totp_setup_required로 막히고, 이 훅이 화면을 등록 절차로 돌린다.
setTOTPSetupHandler(() => {
  if (state.permissions) state.permissions.totpRequired = true;
  showTOTPSetup();
});

// 라우터가 화면을 그린 뒤 메뉴 강조를 갱신한다.
// 라우터 자체는 셸을 모르므로, 경로가 바뀌는 두 경로(뒤로가기, 링크 클릭)에 훅을 붙인다.
window.addEventListener('popstate', () => setTimeout(highlightNav, 0));
document.addEventListener('click', (e) => {
  if (e.target.closest('a[href^="/"]')) setTimeout(highlightNav, 0);
});

async function boot() {
  // 저장된 테마를 먼저 적용한다. index.html의 인라인 스크립트가 이미 적용했지만,
  // 그것이 실패한 경우(CSP 등)에도 첫 화면부터 올바른 팔레트가 나오게 한다.
  theme.init();
  try {
    await loadSession();
  } catch (err) {
    mount(appRoot, h('div.auth-shell', {},
      h('div.auth-card', {},
        icon('alert', 28),
        h('h1', {}, '서버에 연결할 수 없습니다'),
        h('p.auth-sub', {}, err.message),
        h('button.btn.btn-primary.btn-block', { type: 'button', onclick: () => location.reload() }, '다시 시도'),
      ),
    ));
    appRoot.classList.remove('app-loading');
    return;
  }
  router.start();
  await enterApp();
}

boot().catch((err) => {
  toast(`초기화 실패: ${err.message}`, 'error', 0);
});
