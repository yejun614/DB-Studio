-- P27: 마이그레이션 담당자와 리뷰어
--
-- 지금까지 마이그레이션에는 "만든 사람"과 "리뷰한 사람"만 있었다. 둘 다 지나간
-- 사실이라, 계획이 리뷰 대기로 며칠 서 있을 때 "누가 이걸 끌고 가는가", "누구의
-- 확인을 기다리는가"에 답하지 못했다. 답이 화면에 없으면 그 조율은 메신저로
-- 옮겨 가고, 이력에는 남지 않는다.

-- 담당자: 이 마이그레이션을 끝까지 책임지는 한 사람.
--
-- 사람이 지워지면 마이그레이션까지 지울 수는 없으므로(실행 이력이다) SET NULL이다.
-- 담당자 없는 상태는 정상이다 — 아직 정하지 않았다는 뜻이다.
ALTER TABLE migrations ADD COLUMN assignee_id TEXT REFERENCES users (id) ON DELETE SET NULL;

-- 리뷰어: 검토를 부탁받은 사람들.
--
-- migration_reviews(결정)와 다른 표에 두는 이유: 지정은 "봐 달라"는 요청이고
-- 결정은 "봤다"는 기록이다. 요청은 바뀌고 취소되지만 결정은 남아야 한다. 한 표에
-- 섞으면 리뷰어를 바꿀 때마다 지난 승인·반려가 함께 사라진다.
--
-- 승인 수 게이트(RequiredApprovals)는 이 표를 보지 않는다. 지정은 "누구에게
-- 부탁했는가"이고, 실행을 막고 여는 것은 여전히 실제 승인 수다 — 부탁받은 사람이
-- 자리를 비웠다고 배포가 영영 막히면 안 된다.
CREATE TABLE migration_reviewers (
    migration_id TEXT NOT NULL REFERENCES migrations (id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- 누가 지정했는지 남긴다. "왜 내가 리뷰어인가"의 답이 된다.
    added_by     TEXT REFERENCES users (id) ON DELETE SET NULL,
    added_at     TEXT NOT NULL,
    PRIMARY KEY (migration_id, user_id)
);

-- "내가 리뷰어인 마이그레이션"을 찾는 방향의 색인.
CREATE INDEX idx_migration_reviewers_user ON migration_reviewers (user_id, migration_id);
