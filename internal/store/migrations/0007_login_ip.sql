-- 로그인한 접속 IP 기록
--
-- 세션 테이블에도 IP가 있지만(sessions.ip) 세션은 만료되면 지워진다.
-- "마지막으로 로그인한 시각과 그때의 IP"는 세션이 사라진 뒤에도 남아야 하므로
-- 사용자 행에 함께 보관한다. 감사 로그에는 모든 로그인이 남고, 여기에는 최신 하나만 남는다.
ALTER TABLE users ADD COLUMN last_login_ip TEXT NOT NULL DEFAULT '';
