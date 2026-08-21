// 프로필 아이콘.
//
// 그림은 여기에만 있고, 선택 가능한 목록은 서버(`/meta`의 avatars)가 갖는다.
// 목록을 화면이 정하면 서버가 검증할 수 없고, 그림을 서버가 갖게 하면 SVG 문자열이
// API를 타고 흐른다(=신뢰할 수 없는 마크업을 화면에 넣는 습관이 생긴다).
// 그래서 **키는 서버, 획은 화면**으로 나눴다. 서버에만 있는 키는 이니셜로 떨어진다.
//
// 모든 아이콘은 24×24 좌표계, 획 두께 1.6, 채우기 없음으로 그린다. 사이드바에서
// 20px로 줄어들어도 형태가 남아야 하므로 안쪽 디테일은 최소로 유지한다.
import { h, mount } from './dom.js';

const SVG_NS = 'http://www.w3.org/2000/svg';

// 직무 아이콘은 같은 상반신 위에 오른쪽 아래 배지만 바꿔 그린다.
// 실루엣이 같아야 목록이 한 덩어리로 읽히고, 다른 것은 배지 하나로 좁혀진다.
const BUST = [
  'M11 12.4a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7z',
  'M3.8 20.6c0-3.4 3.2-5.6 7.2-5.6 1.1 0 2.2.2 3.1.5',
];

const BADGE = {
  // 데이터베이스 실린더
  dba: [
    'M15.6 16.4c0-.7 1.3-1.2 2.9-1.2s2.9.5 2.9 1.2-1.3 1.2-2.9 1.2-2.9-.5-2.9-1.2z',
    'M15.6 16.4v4c0 .7 1.3 1.2 2.9 1.2s2.9-.5 2.9-1.2v-4',
  ],
  // 코드 꺾쇠
  dev: ['M17.2 15.6 15 18.4l2.2 2.8', 'M19.8 15.6 22 18.4l-2.2 2.8'],
  // 막대 그래프
  analyst: ['M15.8 21.4v-2.6', 'M18.4 21.4v-5.8', 'M21 21.4v-3.8'],
  // 톱니(운영·SRE)
  ops: [
    'M18.4 21.2a2.6 2.6 0 1 0 0-5.2 2.6 2.6 0 0 0 0 5.2z',
    'M18.4 14.4v1.2', 'M18.4 21.4v1.2', 'M14.6 18.6h1.2', 'M21 18.6h1.2',
  ],
  // 층(아키텍처)
  architect: ['M18.4 14.6l3.2 1.8-3.2 1.8-3.2-1.8z', 'M15.2 19.2l3.2 1.8 3.2-1.8'],
  // 방패
  security: ['M18.4 22s3-1.6 3-3.9v-2.5l-3-1.1-3 1.1v2.5c0 2.3 3 3.9 3 3.9z'],
  // 헤드셋
  support: [
    'M15.4 19.2v-.9a3 3 0 0 1 6 0v.9',
    'M15.4 19h1.3v2.5h-1.3z', 'M20.1 19h1.3v2.5h-1.3z',
  ],
  // 체크리스트(기획·관리)
  manager: [
    'M15.2 16.6l.9.9 1.5-1.7', 'M19.2 17h2.6',
    'M15.2 20.4l.9.9 1.5-1.7', 'M19.2 20.8h2.6',
  ],
};

// 사람 아이콘은 얼굴이 주인공이다. 머리 원과 어깨는 공통이고 특징만 얹는다.
const HEAD = 'M12 13.6a5 5 0 1 0 0-10 5 5 0 0 0 0 10z';
const SHOULDERS = 'M3.6 21c0-3.5 3.8-5.8 8.4-5.8s8.4 2.3 8.4 5.8';
const EYES = ['M10.1 7.9h.01', 'M13.9 7.9h.01'];

const PERSON = {
  'person-plain': [HEAD, SHOULDERS, ...EYES],
  'person-glasses': [
    HEAD, SHOULDERS,
    'M10 9.4a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z',
    'M14 9.4a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z',
    'M11.5 7.9h1',
  ],
  'person-beard': [
    HEAD, SHOULDERS, ...EYES,
    // 콧수염(∧)과 짧은 턱수염. 턱선을 따라 그리는 방식은 머리 원과 겹쳐 입처럼 보였고,
    // person-smile(∪)과도 구별되지 않았다. 곡선의 방향이 반대여야 둘이 갈린다.
    'M9.4 11c.8-.8 1.7-1.2 2.6-1.2s1.8.4 2.6 1.2',
    'M10.8 12.6c.4.5.8.7 1.2.7s.8-.2 1.2-.7',
  ],
  'person-cap': [
    HEAD, SHOULDERS, ...EYES,
    // 머리 원(r=5)과 같은 곡률로 그려 머리에 얹힌 것처럼 보이게 한다.
    // 더 큰 반지름으로 그렸을 때는 머리 위로 부풀어 헬멧처럼 보였다.
    'M7.4 6.5A5 5 0 0 1 16.6 6.5', 'M6.8 6.5h11.4',
  ],
  'person-long-hair': [
    HEAD, SHOULDERS, ...EYES,
    'M6.7 8.2v7.4', 'M17.3 8.2v7.4',
  ],
  'person-curly': [
    HEAD, SHOULDERS, ...EYES,
    'M8.6 5.4a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z',
    'M12 4.3a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z',
    'M15.4 5.4a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z',
  ],
  'person-bun': [
    HEAD, SHOULDERS, ...EYES,
    // 머리 원(중심 12,8.5 / 반지름 5)의 오른쪽 위에 닿게 둔다. 떨어뜨리면 안테나로 보인다.
    'M16.4 7a1.9 1.9 0 1 0 0-3.8 1.9 1.9 0 0 0 0 3.8z',
  ],
  'person-smile': [
    HEAD, SHOULDERS, ...EYES,
    'M9.6 10.5c.6.9 1.4 1.4 2.4 1.4s1.8-.5 2.4-1.4',
  ],
};

// 키 → 획 목록. 직무 아이콘은 공통 상반신 + 배지로 조립한다.
const AVATAR_PATHS = { ...PERSON };
for (const [name, badge] of Object.entries(BADGE)) {
  AVATAR_PATHS[`role-${name}`] = [...BUST, ...badge];
}

// hasAvatarIcon은 이 키를 그릴 수 있는지 알려준다.
// 서버 목록에 있는데 여기 없으면 화면이 조용히 빈 칸을 그리는 대신 이니셜로 떨어진다.
export function hasAvatarIcon(key) {
  return Boolean(key && AVATAR_PATHS[key]);
}

// avatarIcon은 아이콘 SVG를 만든다. 키를 그릴 수 없으면 null이다.
export function avatarIcon(key, size = 20) {
  const paths = AVATAR_PATHS[key];
  if (!paths) return null;
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('width', size);
  svg.setAttribute('height', size);
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '1.6');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.classList.add('icon');
  for (const d of paths) {
    const p = document.createElementNS(SVG_NS, 'path');
    p.setAttribute('d', d);
    svg.appendChild(p);
  }
  return svg;
}

// initial은 이름에서 한 글자를 뽑는다. 아이콘을 고르지 않은 사용자의 표시다.
export function initial(user) {
  const src = user?.displayName || user?.username || '?';
  return src.trim().slice(0, 1).toUpperCase() || '?';
}

// 업로드한 이미지를 쓰는 사용자의 avatar 값.
// 서버(internal/model/avatar.go)의 AvatarUpload와 같은 문자열이어야 한다.
export const AVATAR_UPLOAD = 'upload';

// avatarImageURL은 업로드 이미지의 주소다.
//
// v(버전)를 붙이는 이유: 경로가 사용자 ID로만 이루어지면 사진을 바꿔도 브라우저가
// 캐시된 옛 이미지를 계속 보여준다. 서버는 이 이미지를 하루 동안 캐시하라고 알려주므로
// 무효화할 방법이 필요하다.
export function avatarImageURL(user) {
  if (!user?.id) return null;
  return `/api/v1/users/${encodeURIComponent(user.id)}/avatar?v=${user.avatarVersion ?? 0}`;
}

// avatarNode는 사용자 한 명을 나타내는 원형 요소를 만든다.
// 화면 여러 곳(사이드바·사용자 목록·프로필)이 같은 함수를 쓰므로,
// 아이콘이 있고 없을 때의 모양 차이가 한 곳에서만 결정된다.
export function avatarNode(user, { size = 32, className = '', previewURL = null } = {}) {
  const el = h(`div.avatar${className ? `.${className}` : ''}`, {
    style: { width: `${size}px`, height: `${size}px` },
  });

  // previewURL은 아직 저장하지 않은 이미지를 미리 보여줄 때 쓴다(프로필 화면).
  const src = previewURL ?? (user?.avatar === AVATAR_UPLOAD ? avatarImageURL(user) : null);
  if (src) {
    const img = h('img.avatar-img', { src, alt: '' });
    // 이미지를 못 불러오면 이니셜로 떨어진다. 깨진 이미지 아이콘을 보여주면
    // 사용자는 자기 계정이 잘못된 줄 안다.
    img.addEventListener('error', () => {
      el.classList.remove('avatar-photo');
      mount(el, initial(user));
    });
    el.classList.add('avatar-photo');
    el.appendChild(img);
    return el;
  }

  const svg = avatarIcon(user?.avatar, Math.round(size * 0.62));
  if (svg) el.classList.add('avatar-icon');
  el.appendChild(svg ?? document.createTextNode(initial(user)));
  return el;
}
