// 앱 엔트리포인트: 세션 확인 → 셸 구성 → 라우팅.
import {
  api, setUnauthorizedHandler, setPasswordChangeHandler, setTOTPSetupHandler,
} from './core/api.js';
import { state, loadSession, loadMeta, clearSession, subscribe } from './core/store.js';
import { h, mount, icon, toast, toastError, openModal, badge } from './core/ui.js';
import * as router from './core/router.js';
import * as theme from './core/theme.js';
import { avatarNode } from './core/avatars.js';
import * as sidebar from './core/sidebar.js';
import {
  loadProjects, projects, currentProjectID, setCurrentProject, onProjectChange,
} from './core/project.js';
import { bindPalette, openPalette } from './core/palette.js';
import { closeAllFloatPanels } from './core/floatpanel.js';
import { setScreenNames } from './core/screen.js';
import { renderLogin, renderPasswordChange, renderChangePasswordPage } from './pages/login.js';
import { renderDashboard } from './pages/dashboard.js';
import { renderUsers } from './pages/users.js';
import { renderProfile } from './pages/profile.js';
import { renderAccess } from './pages/access.js';
import { renderConnections } from './pages/connections.js';
import { renderProjects } from './pages/projects.js';
import { renderSchema } from './pages/schema.js';
import { renderMonitor, renderEvents } from './pages/monitor.js';
import { renderRules } from './pages/rules.js';
import { renderHost } from './pages/host.js';
import { renderLogs } from './pages/logs.js';
import { renderERDList } from './pages/erd.js';
import { renderStructure } from './pages/structure.js';
import { renderERDEditor } from './pages/erdeditor.js';
import { renderMigrations, renderMigrationDetail, renderVersions } from './pages/migrations.js';
import { renderGlossary } from './pages/glossary.js';
import { renderVCS } from './pages/vcs.js';
import { renderNoSQL } from './pages/nosql.js';
import { renderData } from './pages/data.js';
import { renderSQLConsole } from './pages/sqlconsole.js';
import { renderBackups } from './pages/backups.js';
import { renderMacros, renderMacroRuns } from './pages/macros.js';
import { renderMacroEditor } from './pages/macroeditor.js';
import { renderTriggers } from './pages/triggers.js';
import {
  renderAssistant, toggleAssistantPopup, confirmAssistantAbort,
} from './pages/assistant.js';
import { renderManual } from './pages/manual.js';
import { renderAbout } from './pages/about.js';
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
router.define('/projects', renderProjects);
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
router.define('/glossary', renderGlossary);
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
router.define('/about', renderAbout);
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
      { path: '/projects', label: '프로젝트', icon: 'box' },
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
      // 용어 사전은 설계하다 찾아보는 것이라 ERD 옆에 둔다. 관리 묶음에 두면
      // "설정"처럼 보이는데, 이것은 설정이 아니라 설계하는 사람이 매일 여는 표다.
      { path: '/glossary', label: '용어 사전', icon: 'list' },
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
      // 정보는 맨 아래다. 자주 여는 화면은 아니지만, 문제를 보고할 때 가장 먼저
      // 필요한 것(무엇이 돌고 있는가)이 여기 있다.
      { path: '/about', label: '정보', icon: 'box' },
    ],
  },
];

// 화면 이름은 메뉴에서 가져간다(어시스턴트에게 "지금 어느 화면인지"를 알려줄 때 쓴다).
// 표를 따로 두면 메뉴 이름을 고칠 때 한쪽만 고쳐지고, 모델은 없는 화면 이름을 말한다.
setScreenNames(NAV);

// paletteButton은 명령 팔레트로 가는 눈에 보이는 입구다.
//
// 단축키만 두지 않는 이유: 단축키는 아는 사람만 쓴다. 사이드바 맨 위에 검색 상자
// 모양으로 두면, 그것을 눌러 본 사람이 그 자리에서 단축키도 알게 된다.
function paletteButton() {
  const mac = /mac/i.test(navigator.userAgent);
  return h('button.palette-open', {
    type: 'button',
    onclick: () => openPalette(NAV, paletteOptions()),
  },
  icon('list', 15),
  h('span', {}, '찾기'),
  h('kbd', {}, mac ? '⌘K' : 'Ctrl K'));
}

// projectSwitcher는 사이드바 맨 위에서 보고 있는 프로젝트를 고른다.
//
// 고르는 자리를 여기 하나만 두는 이유: 자원은 모두 프로젝트 안에 있다. 화면마다
// 고르게 하면 같은 사람이 같은 시간에 서로 다른 프로젝트를 보게 되고, "왜 여기엔
// 없지"가 끝없이 생긴다. 브랜드 바로 아래에 두어 "지금 어디를 보고 있는가"가 다른
// 무엇보다 먼저 읽히게 한다.
let unsubscribeProject = null;

function projectSwitcher() {
  // 셸을 다시 만들 때(로그아웃 후 재로그인) 이전 구독을 버린다.
  unsubscribeProject?.();
  const box = h('div.project-switch');

  const render = () => {
    const list = projects();
    if (list.length === 0) {
      // 프로젝트가 없으면 고를 것도 없다. 그 자리에 만드는 입구를 둔다 —
      // 자원을 만들려면 프로젝트가 먼저라는 사실을 처음 만나는 자리다.
      mount(box, h('a.project-switch-empty', { href: '/projects' },
        icon('box', 15), h('span', {}, '프로젝트 만들기')));
      return;
    }
    const now = list.find((p) => p.id === currentProjectID()) ?? list[0];
    // 고르개(select)가 아니라 단추다.
    //
    // 사이드바는 좁아서 고르개에는 이름 말고 아무것도 들어가지 않는다. 프로젝트를
    // 바꾸는 것은 화면의 모든 내용을 갈아 치우는 일인데, 이름만 스치듯 보고 고르면
    // "왜 여기엔 없지"가 그 다음에 온다. 창을 열면 설명과 규모(서버·DB·초안·참여자)를
    // 함께 보고 고를 수 있고, 잘못 열었으면 취소할 수 있다.
    mount(box, h('button.project-switch-btn', {
      type: 'button',
      title: '보고 있는 프로젝트 — 눌러서 바꿉니다',
      onclick: () => openProjectDialog(),
    }, icon('box', 15), h('span.project-switch-name', {}, now?.name ?? '프로젝트'),
    icon('chevron-down', 13)),
    h('a.project-switch-more', { href: '/projects', title: '프로젝트 관리' },
      icon('settings', 14)));
  };

  render();
  unsubscribeProject = onProjectChange(render);
  return box;
}

// openProjectDialog는 프로젝트를 고르는 창이다.
//
// 고른 즉시 닫고 화면을 다시 그린다. "고르기 → 확인"의 두 걸음을 두지 않는 이유:
// 고르는 항목이 곧 결정이고, 잘못 눌렀으면 다시 열어 바꾸면 된다. 대신 아무것도
// 고르지 않고 나갈 길(취소·Esc·X)은 남겨 둔다 — 창을 연 이유의 절반은 "지금 어디를
// 보고 있는지" 확인하는 것이다.
function openProjectDialog() {
  const list = projects();
  const currentID = currentProjectID();

  const close = openModal({
    title: '프로젝트 고르기',
    width: 520,
    body: () => [
      h('p.field-help', {},
        'DB 서버·초안·마이그레이션·용어 사전은 모두 프로젝트 안에 있습니다. '
        + '고르면 화면의 내용이 그 프로젝트의 것으로 바뀝니다.'),
      h('div.project-pick-list', {}, list.map((p) => {
        const isCurrent = p.id === currentID;
        const row = h(`button.project-pick${isCurrent ? '.is-current' : ''}`, {
          type: 'button',
          // 지금 보고 있는 것에 포커스를 준다. 창을 열자마자 방향키로 위아래를
          // 훑을 수 있고, "지금 여기"가 어디인지도 초점으로 한 번 더 보인다.
          // 빈 문자열이 아니라 true 다. autofocus 는 요소의 속성(property)이라
          // h() 가 el.autofocus 에 그대로 넣는데, '' 는 거짓이라 아무 일도
          // 일어나지 않는다(그래서 초점이 대화상자에 남았다).
          autofocus: isCurrent ? true : null,
          onclick: async () => {
            close();
            if (isCurrent) return;
            if (!setCurrentProject(p.id)) return;
            // 이름 뒤에 조사를 붙이지 않는다. 받침에 따라 "로/으로"가 갈리고,
            // 프로젝트 이름에 "프로젝트"가 들어 있으면 말이 두 번 겹친다
            // ("검사 프로젝트 프로젝트로").
            toast(`프로젝트를 바꿨습니다 — ${p.name}`, 'success');
            // 프로젝트가 바뀌면 지금 화면의 내용이 통째로 달라진다. 다시 그리지
            // 않으면 남의 프로젝트 자료를 그대로 보고 있게 된다.
            await router.render();
            highlightNav();
          },
        },
          h('div.project-pick-main', {},
            h('span.project-pick-name', {}, p.name),
            p.note ? h('span.project-pick-note', {}, p.note) : null,
            h('span.project-pick-stat', {},
              `서버 ${p.servers ?? 0} · DB ${p.connections ?? 0} · `
              + `초안 ${p.documents ?? 0} · 참여자 ${p.members ?? 0}`)),
          isCurrent ? badge('보는 중', 'ok') : icon('chevron-right', 14),
        );
        return row;
      })),
    ],
    footer: (closeFn) => [
      h('a.btn.btn-small', { href: '/projects', onclick: () => closeFn() },
        icon('settings', 13), '프로젝트 관리'),
      h('span.modal-foot-spacer'),
      h('button.btn', { type: 'button', onclick: closeFn }, '취소'),
    ],
  });
}

// versionLine은 사이드바 아래에 지금 돌고 있는 버전을 적는다.
//
// 값을 /meta 에서 가져오는 이유: 셸을 그리기 전에 이미 한 번 받아 두는 응답이다.
// 여기서 따로 /health 를 부르면 화면을 열 때마다 요청이 하나 늘고, 그 요청이
// 늦으면 버전 줄만 뒤늦게 나타난다.
function versionLine() {
  const build = state.meta?.build;
  if (!build?.version) return null;
  const label = build.version === 'dev' ? '개발 빌드' : build.version;
  return h('a.sidebar-version', {
    href: '/about',
    title: `${build.version}${build.commit ? ` (${build.commit})` : ''} · ${build.platform ?? ''}`,
  }, label);
}

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
          onclick: () => toggleAssistantPopup(),
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
      h('div.sidebar-top', {},
        h('a.brand', { href: '/' }, icon('database', 22), h('span', {}, 'DB Studio')),
        // 접기 단추는 브랜드 옆에 둔다. 사이드바의 위쪽 모서리는 "이 칸 자체를
        // 어떻게 할지"를 정하는 자리이고, 아래 메뉴들과 섞이면 메뉴처럼 읽힌다.
        sidebar.createHideButton(),
      ),
      projectSwitcher(),
      paletteButton(),
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
        // 버전을 늘 보이는 자리에 둔다. 누르면 정보 화면으로 간다.
        //
        // 메뉴에만 두지 않는 이유: 배포가 실제로 바뀌었는지는 화면을 열자마자
        // 확인하고 싶은 것이고, 그때 메뉴를 찾아 들어가는 단계가 있으면
        // 대개 확인하지 않는다.
        versionLine(),
      ),
      sidebar.createResizer(),
    ),
    sidebar.createBackdrop(),
    // 접었을 때만 보이는 손잡이. 사이드바 밖에 두어야 사이드바가 사라져도 남는다.
    sidebar.createShowButton(),
    outlet,
  );

  mount(appRoot, shell);
  appRoot.classList.remove('app-loading');
  sidebar.bindGlobalHandlers();
  bindCommandPalette();
  router.setOutlet(outlet);
  return outlet;
}

// 명령 팔레트를 연결한다.
//
// 셸을 다시 만들 때(로그아웃 후 재로그인) 이전 연결을 버린다. 버리지 않으면 로그인마다
// 리스너가 하나씩 쌓여 Ctrl+K 한 번에 팔레트가 여러 번 토글된다(= 열리지 않는다).
let unbindPalette = null;

function bindCommandPalette() {
  unbindPalette?.();
  unbindPalette = bindPalette(NAV, paletteOptions());
}

// paletteOptions는 팔레트가 바깥 세계와 닿는 지점이다.
//
// 동작 목록에 되돌릴 수 없는 것을 넣지 않는다. 팔레트는 눈으로 확인하지 않고 Enter를
// 누르는 곳이라, 지우는 일이 여기 있으면 손이 미끄러지는 자리가 된다. 로그아웃은
// 예외로 둔다 — 되돌릴 수 없지만 잃는 것이 없고, 자주 찾는 동작이다.
function paletteOptions() {
  return {
    navigate: (path) => router.navigate(path),
    onPopup: () => toggleAssistantPopup(),
    actions: [
      {
        id: 'assistant',
        label: 'AI 어시스턴트 열기 / 닫기',
        icon: 'sparkles',
        keywords: 'ai assistant 어시스턴트 물어보기',
        run: () => toggleAssistantPopup(),
      },
      {
        id: 'theme.light',
        label: '밝은 테마',
        icon: 'sun',
        keywords: 'theme light 라이트 밝게',
        run: () => theme.applyMode('light'),
      },
      {
        id: 'theme.dark',
        label: '어두운 테마',
        icon: 'moon',
        keywords: 'theme dark 다크 어둡게',
        run: () => theme.applyMode('dark'),
      },
      {
        id: 'theme.system',
        label: '테마: 시스템 설정 따르기',
        icon: 'monitor',
        keywords: 'theme system 시스템',
        run: () => theme.applyMode('system'),
      },
      {
        id: 'logout',
        label: '로그아웃',
        icon: 'logout',
        keywords: 'logout signout 나가기',
        run: () => logout(),
      },
    ],
  };
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
  // 받고 있는 AI 답변이 있으면 먼저 묻는다.
  //
  // 로그아웃은 어시스턴트를 지나지 않는다 — 사이드바 단추가 셸을 통째로 내리고,
  // 그때 떠 있던 창도 함께 닫힌다(closeAllFloatPanels). 그래서 팝업의 X 와
  // 화면 이동은 물어보는데 로그아웃만 조용히 답변을 끊고 있었다.
  if (!(await confirmAssistantAbort())) return;
  try {
    await api.post('/auth/logout');
  } catch {
    // 서버 오류와 무관하게 클라이언트 상태는 초기화한다.
  }
  clearSession();
  showLogin();
}

// ---------- 화면 전환 ----------

// leaveShell은 앱 셸을 내리고 인증 화면으로 바꿀 준비를 한다.
//
// 세 화면(로그인·비밀번호 강제 변경·2단계 인증 등록)이 같은 자리를 쓴다. 셋 다
// **셸 밖**이므로, 셸에 매이지 않은 것들을 여기서 함께 치운다 — 떠 있는 창은
// document.body 에 붙어 있어서 #app 을 갈아 끼워도 살아남는다.
function leaveShell() {
  appRoot.classList.remove('app-loading');
  router.setOutlet(null);
  closeAllFloatPanels();
}

function showLogin() {
  leaveShell();
  renderLogin(appRoot, { onSuccess: enterApp });
}

function showPasswordChange() {
  leaveShell();
  renderPasswordChange(appRoot, {
    onSuccess: enterApp,
    // 이 화면은 앱 전체를 가리고 서 있다. 나갈 문이 없으면 임시 비밀번호를 받아 든
    // 사람이 한 번 로그인한 컴퓨터에서, 그 컴퓨터의 주인이 자기 계정으로 들어갈
    // 방법이 없어진다. 비밀번호를 바꾸지 않고 나가는 것은 강제를 우회하는 것이
    // 아니다 — 그 계정으로 다시 들어오면 이 화면이 그대로 다시 선다.
    onSwitchUser: logout,
  });
}

// showTOTPSetup은 2단계 인증 의무화 상태에서 등록을 마칠 때까지 다른 화면을 막는다.
// 비밀번호 강제 변경과 같은 자리, 같은 처리다.
function showTOTPSetup() {
  leaveShell();
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
    // 프로젝트를 셸보다 먼저 읽는다. 사이드바의 고르개와 첫 화면이 같은 답을
    // 써야 하고, 나중에 읽으면 첫 화면이 "프로젝트 없음"으로 한 번 깜빡인다.
    await loadProjects();
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
