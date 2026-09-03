// History API 기반 라우터. 경로 → 렌더 함수 매핑만 담당한다.
import { clearScreenDetail } from './screen.js';

const routes = [];
let outlet = null;
let notFound = null;
let currentCleanup = null;
// 마지막으로 그린 주소. 뒤로 가기를 되돌릴 때 쓴다.
let lastPath = '';

// define('/users/:id', render) 형태로 등록한다.
export function define(pattern, render, options = {}) {
  const keys = [];
  const regexSource = pattern
    .split('/')
    .map((seg) => {
      if (seg.startsWith(':')) {
        keys.push(seg.slice(1));
        return '([^/]+)';
      }
      return seg.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    })
    .join('/');
  routes.push({ regex: new RegExp(`^${regexSource}/?$`), keys, render, options });
}

export function setOutlet(el) {
  outlet = el;
}

export function setNotFound(render) {
  notFound = render;
}

// 떠나기 전에 물어볼 함수. 편집 중인 화면이 등록한다.
// false를 돌려주면 이동을 취소한다. 화면이 바뀔 때 자동으로 풀린다.
let leaveGuard = null;

export function setLeaveGuard(fn) {
  leaveGuard = fn;
}

async function canLeave() {
  if (!leaveGuard) return true;
  try {
    return (await leaveGuard()) !== false;
  } catch {
    // 확인 과정이 실패했다고 사용자를 가둘 수는 없다.
    return true;
  }
}

export async function navigate(path, { replace = false } = {}) {
  if (path === currentPath() && !replace) {
    render();
    return;
  }
  if (!(await canLeave())) return;
  if (replace) history.replaceState({}, '', path);
  else history.pushState({}, '', path);
  render();
}

export function currentPath() {
  return location.pathname + location.search;
}

export function match(path) {
  const clean = path.split('?')[0];
  for (const route of routes) {
    const m = route.regex.exec(clean);
    if (m) {
      const params = {};
      route.keys.forEach((key, i) => {
        params[key] = decodeURIComponent(m[i + 1]);
      });
      return { route, params };
    }
  }
  return null;
}

export async function render() {
  if (!outlet) return;
  // 화면이 바뀌면 이탈 확인도 함께 풀린다. 다음 화면까지 남으면
  // 엉뚱한 곳에서 "저장하시겠습니까"가 뜬다.
  leaveGuard = null;
  // 어시스턴트에게 알려 줄 화면 정보도 비운다. 남으면 떠난 화면의 테이블·쿼리가
  // 다음 화면의 것으로 보고된다.
  clearScreenDetail();
  lastPath = currentPath();
  // 이전 화면이 등록한 타이머/리스너를 해제한다.
  if (currentCleanup) {
    try {
      currentCleanup();
    } catch {
      // 정리 실패가 다음 화면 렌더를 막지 않도록 무시한다.
    }
    currentCleanup = null;
  }

  const found = match(location.pathname);
  const query = new URLSearchParams(location.search);
  if (!found) {
    currentCleanup = (await notFound?.(outlet, {}, query)) ?? null;
    return;
  }
  currentCleanup = (await found.route.render(outlet, found.params, query)) ?? null;
}

export function start() {
  // 뒤로 가기는 주소가 이미 바뀐 뒤에 온다. 이탈을 취소하면 원래 주소로 되돌린다.
  window.addEventListener('popstate', async () => {
    if (!(await canLeave())) {
      history.pushState({}, '', lastPath);
      return;
    }
    render();
  });

  // 앱 내부 링크는 전체 페이지 로드 없이 라우팅한다.
  document.addEventListener('click', (e) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const anchor = e.target.closest('a[href]');
    if (!anchor) return;
    const href = anchor.getAttribute('href');
    if (!href || !href.startsWith('/') || anchor.target === '_blank' || anchor.hasAttribute('download')) return;
    e.preventDefault();
    navigate(href);
  });

  render();
}
