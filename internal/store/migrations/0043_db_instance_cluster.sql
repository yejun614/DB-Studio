-- 클러스터로 만든 DB 인스턴스.
--
-- ── 왜 줄을 노드마다 두는가 ─────────────────────────────────────────
-- 클러스터 하나를 줄 하나로 담을 수도 있었다. 그러면 컨테이너 목록을 한 칸에
-- 넣게 되는데, 그 순간 실행·중단·로그·상태가 전부 "그 줄의 몇 번째"를 다시
-- 물어야 하는 일이 된다. 노드마다 줄을 두면 지금 있는 것이 그대로 쓰인다 —
-- 목록도, 시작도, 로그도, 지우기도.
--
-- 묶음이라는 사실은 cluster 칸이 말한다. 같은 값이면 한 클러스터다.
--
-- ── 왜 shard·replica 를 적어 두는가 ─────────────────────────────────
-- 이름(x-s1r2)에서 뽑아낼 수도 있지만, 이름을 파싱해서 얻은 숫자는 이름 규칙을
-- 바꾸는 순간 조용히 틀린다. 그리고 이 두 값은 노드의 macros 설정과 같아야
-- 하는 것이라, 우리가 정한 값을 그대로 들고 있어야 다시 만들 때도 같은 자리에
-- 놓인다.

ALTER TABLE db_instances ADD COLUMN cluster TEXT NOT NULL DEFAULT '';

-- role 은 '' (단독) · 'keeper' (조정자) · 'node' (데이터 노드) 다.
--
-- 조정자를 따로 표시하는 이유: 그것은 DB 가 아니다. 커넥션으로 등록하지 않고,
-- 목록에서도 다르게 보여야 한다("접속할 수 없는 줄"이 하나 섞여 있으면 사람은
-- 그것을 고장으로 읽는다).
ALTER TABLE db_instances ADD COLUMN role TEXT NOT NULL DEFAULT '';

ALTER TABLE db_instances ADD COLUMN shard INTEGER NOT NULL DEFAULT 0;
ALTER TABLE db_instances ADD COLUMN replica INTEGER NOT NULL DEFAULT 0;

-- 한 프로젝트 안에서 클러스터 이름으로 묶어 찾는다.
CREATE INDEX idx_db_instances_cluster ON db_instances (project_id, cluster);
