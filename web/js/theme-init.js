// 저장된 테마를 첫 페인트 전에 적용한다.
//
// 별도 파일인 이유: CSP가 `script-src 'self'`라서 인라인 스크립트는 실행되지 않는다.
// 예외를 두는 대신(unsafe-inline은 XSS 방어를 무력화하고, 해시는 파일을 고칠 때마다
// 서버 헤더를 함께 고쳐야 한다) 같은 출처의 파일로 옮겼다.
//
// 모듈이 아닌 이유: type="module"은 defer와 같아서 문서 파싱 후에 실행되고,
// 그때는 이미 반대 팔레트로 한 번 그려져 있다. head의 동기 스크립트여야 한다.
//
// 키 문자열이 core/theme.js와 중복된다. 클래식 스크립트는 import를 쓸 수 없어
// 공유할 방법이 없다 — 한쪽을 바꾸면 다른 쪽도 바꿔야 한다.
(function () {
  try {
    var mode = localStorage.getItem('dbstudio-theme');
    if (mode === 'light' || mode === 'dark') {
      document.documentElement.setAttribute('data-theme', mode);
    }
  } catch (e) {
    // localStorage가 막힌 환경(사생활 보호 모드)에서는 OS 설정을 따른다.
  }
})();
