// 정보 화면 — 지금 이 서버에서 돌고 있는 빌드.
//
// 이 화면이 있는 이유: 문제를 보고할 때 가장 먼저 필요한 것이 "무엇이 돌고 있는가"다.
// 버전을 터미널 로그나 /api/v1/health 로만 알 수 있으면, 화면을 쓰는 사람에게는
// 확인할 방법이 없다. 그러면 보고는 "어제부터 안 돼요"에서 멈춘다.
//
// 값을 /health 에서 받는 이유: 그 응답이 이미 빌드 정보를 담고 있고 로그인 없이
// 열려 있어(배포 검증 스크립트가 쓴다) 권한을 따질 필요가 없다. 같은 값을 두 곳에서
// 만들면 화면과 스크립트가 다른 버전을 말하는 일이 생긴다.
import { api } from '../core/api.js';
import {
  h, mount, icon, spinner, badge, pageHeader,
  formatDate, relativeTime, copyToClipboard, toast,
} from '../core/ui.js';
import { errorPanel } from './users.js';

const REPO = 'https://github.com/yejun614/DB-Studio';

export async function renderAbout(outlet) {
  mount(outlet, spinner('버전 정보를 불러오는 중…'));

  let health;
  try {
    health = await api.get('/health');
  } catch (err) {
    mount(outlet, pageHeader('정보', '이 서버에서 돌고 있는 빌드'), errorPanel(err));
    return;
  }
  const build = health.build ?? {};

  // 클러스터면 어느 노드를 보고 있는지 함께 알려준다.
  //
  // 실패를 삼키는 이유: 노드 목록은 슈퍼 어드민만 볼 수 있다. 권한이 없거나
  // 단일 서버면 이 줄이 없을 뿐이고, 버전 확인은 그대로 되어야 한다.
  let cluster = null;
  try {
    const res = await api.get('/cluster/');
    if (res?.status?.enabled) cluster = res.status;
  } catch { /* 권한 없음 또는 단일 서버 */ }

  // 보고에 붙일 한 줄. 사람이 화면을 옮겨 적는 단계를 없앤다.
  const oneLine = [
    `DB Studio ${build.version ?? '?'}`,
    build.commit ? `(${build.commit})` : '',
    build.platform ?? '',
    build.goVersion ?? '',
    cluster ? `· ${cluster.role === 'master' ? '마스터' : '리플리카'} ${cluster.nodeName}` : '',
  ].filter(Boolean).join(' ');

  mount(outlet,
    pageHeader('정보', '이 서버에서 돌고 있는 빌드'),

    h('div.card', {},
      h('div.card-title', {},
        h('span', {}, '버전'),
        build.version === 'dev'
          ? badge('개발 빌드', 'warn')
          : badge(build.version ?? '알 수 없음', 'info'),
      ),
      h('dl.cluster-meta', {},
        metaRow('버전', build.version ?? '-'),
        metaRow('커밋', build.commit || '(빌드에 심어지지 않음)'),
        metaRow('빌드 시각', build.buildDate
          ? `${formatDate(build.buildDate)} (${relativeTime(build.buildDate)})`
          : '-'),
        metaRow('Go', build.goVersion ?? '-'),
        metaRow('플랫폼', build.platform ?? '-'),
        metaRow('서버 시각', health.time ? formatDate(health.time) : '-'),
        cluster ? metaRow('이 노드',
          `${cluster.nodeName} · ${cluster.role === 'master' ? '마스터' : '리플리카'}`) : null,
      ),
      h('p.field-help', {},
        '문제를 보고할 때 이 한 줄을 함께 적어 주세요 — 어느 빌드에서 생긴 일인지가 '
        + '조사의 출발점입니다. 오른쪽 위 버튼으로 복사합니다.'),
      // 복사 버튼은 코드블럭 안에 둔다. 설명서의 코드 복사 버튼과 같은 장치이므로
      // 같은 클래스를 쓴다 — 두 화면이 다르게 생기면 같은 기능으로 읽히지 않는다.
      h('div.md-code-wrap', {},
        h('pre.code-block', {}, oneLine),
        h('button.icon-btn.md-code-copy', {
          type: 'button',
          title: '버전 정보 복사',
          'aria-label': '버전 정보 복사',
          onclick: () => {
            copyToClipboard(oneLine);
            toast('버전 정보를 복사했습니다', 'success');
          },
        }, icon('copy', 14)),
      ),
    ),

    h('div.card', {},
      h('h2.card-title', {}, '문서와 도움'),
      // 키·값 표(.cluster-meta)를 쓰지 않는 이유: 그 표의 값 칸은 고정폭 글꼴이고
      // 한 줄로 잘린다. 주소에는 맞지만 설명이 붙는 링크 목록에는 맞지 않는다.
      h('ul.about-links', {},
        aboutLink(h('a.link', { href: '/manual' }, '사용 설명서'),
          '이 바이너리에 함께 들어 있습니다 — 화면과 설명이 다르면 화면이 맞습니다'),
        aboutLink(extLink(REPO, '저장소'), '소스와 문서'),
        aboutLink(extLink(`${REPO}/releases`, '릴리스'), '플랫폼별 바이너리와 변경 내역'),
        aboutLink(extLink(`${REPO}/issues`, '문제 보고'), '위의 한 줄을 함께 적어 주세요'),
      ),
    ),

    h('div.card', {},
      h('h2.card-title', {}, '라이선스'),
      h('p', {}, 'DB Studio는 MIT 라이선스로 배포됩니다.'),
      h('p.field-help', {},
        '함께 담긴 본문 글꼴 Pretendard JP는 SIL Open Font License 1.1을 따릅니다. '
        + 'CDN을 쓸 수 없는 사설망에서도 글꼴이 깨지지 않도록 바이너리에 넣었습니다.'),
    ),
  );

  return () => {};
}

function metaRow(key, value) {
  return h('div.meta-row', {}, h('dt', {}, key), h('dd', {}, value));
}

function aboutLink(anchor, note) {
  return h('li', {}, anchor, h('span.muted', {}, note));
}

// 바깥으로 나가는 링크는 새 탭으로 연다. 보던 화면을 잃지 않게 한다.
function extLink(url, label) {
  return h('a.link', {
    href: url, target: '_blank', rel: 'noreferrer noopener',
  }, label, icon('share', 12));
}
