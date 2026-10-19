// 아직 반영되지 않은 수정이 있을 때 떠나기 전에 묻는다.
//
// 길이 둘이다. 새로고침·창 닫기·주소 입력은 beforeunload 로만 잡히고, 앱 안에서 메뉴를
// 눌러 옮기는 것은 브라우저 입장에서 이동이 아니라서 라우터의 이탈 확인으로 잡는다.
// 한쪽만 걸면 "탭을 닫을 때는 묻는데 메뉴를 누르면 그냥 사라진다"가 된다.
//
// 브라우저는 beforeunload 에 우리 문장을 보여 주지 않는다(자기 문구를 쓴다). 그래서
// 무엇이 사라지는지는 앱 안 이동의 확인 창에서만 말할 수 있다.
import { setLeaveGuard } from './router.js';
import { confirmDialog } from './ui.js';

// guardUnsaved는 count()가 0보다 큰 동안 떠나기 전에 묻게 한다. 돌려준 함수로 푼다.
// describe(n)는 확인 창에 보일 문장이다.
export function guardUnsaved(count, describe) {
  const onBeforeUnload = (e) => {
    if (count() <= 0) return;
    e.preventDefault();
    // 값을 채워야 묻는 브라우저가 있다.
    e.returnValue = '';
  };
  window.addEventListener('beforeunload', onBeforeUnload);
  setLeaveGuard(async () => {
    const n = count();
    if (n <= 0) return true;
    return confirmDialog({
      title: '반영되지 않은 수정이 있습니다',
      message: describe(n),
      confirmLabel: '버리고 나가기',
      danger: true,
    });
  });
  return () => {
    window.removeEventListener('beforeunload', onBeforeUnload);
    setLeaveGuard(null);
  };
}
