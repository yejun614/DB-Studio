// 지금 보고 있는 프로젝트.
//
// 자원은 모두 프로젝트 안에 있다(커넥션·ERD·마이그레이션·버전·용어 사전). 화면마다
// 프로젝트를 고르게 하면 같은 사람이 같은 시간에 서로 다른 프로젝트를 보게 되고,
// "왜 여기엔 없지"가 끝없이 생긴다. 그래서 고르는 자리는 사이드바 하나뿐이고,
// 모든 화면이 여기서 답을 얻는다.
import { api } from './api.js';

const KEY = 'dbstudio.project';

let list = [];
let currentID = null;
let canManage = false;
const listeners = new Set();

// stored는 마지막으로 고른 프로젝트를 기억한다.
//
// 기억하지 않으면 화면을 새로 고칠 때마다 첫 프로젝트로 돌아간다 — 하루 종일 한
// 프로젝트에서 일하는 사람에게 그것은 매번 다시 고르는 일이 된다.
function stored() {
  try {
    return localStorage.getItem(KEY) ?? '';
  } catch {
    // 사생활 보호 모드 등에서 막힐 수 있다. 기억하지 못할 뿐 동작은 해야 한다.
    return '';
  }
}

function remember(id) {
  try {
    if (id) localStorage.setItem(KEY, id);
    else localStorage.removeItem(KEY);
  } catch {
    // 위와 같다.
  }
}

// loadProjects는 볼 수 있는 프로젝트를 다시 읽고 지금 선택을 맞춘다.
//
// 고른 프로젝트가 목록에서 사라졌으면(참여가 끊겼거나 지워졌으면) 첫 번째로
// 옮긴다. 없는 것을 계속 가리키면 모든 화면이 빈 채로 남고, 그 이유는 어디에도
// 적혀 있지 않다.
export async function loadProjects() {
  const res = await api.get('/projects/');
  list = res.projects ?? [];
  canManage = res.canManage === true;

  const want = currentID || stored();
  const found = list.find((p) => p.id === want);
  currentID = found ? found.id : (list[0]?.id ?? null);
  remember(currentID);
  notify();
  return list;
}

function notify() {
  for (const fn of listeners) fn(currentProject());
}

export function onProjectChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function projects() {
  return list;
}

export function currentProjectID() {
  return currentID;
}

export function currentProject() {
  return list.find((p) => p.id === currentID) ?? null;
}

export function canManageProjects() {
  return canManage;
}

export function hasProjects() {
  return list.length > 0;
}

// setCurrentProject는 고른 프로젝트를 바꾼다. 실제로 바뀐 경우에만 알린다.
export function setCurrentProject(id) {
  if (!id || id === currentID) return false;
  currentID = id;
  remember(id);
  notify();
  return true;
}

// withProject는 목록 요청에 지금 프로젝트를 붙인다.
//
// 서버는 이 값이 없으면 "볼 수 있는 전체"로 답한다. 그것도 안전하지만 화면이
// 말하는 것과 다르므로, 목록을 부르는 쪽은 언제나 이 함수를 지난다.
export function withProject(path) {
  if (!currentID) return path;
  const sep = path.includes('?') ? '&' : '?';
  return `${path}${sep}project=${encodeURIComponent(currentID)}`;
}
