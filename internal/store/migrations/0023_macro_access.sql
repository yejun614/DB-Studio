-- P23: 매크로 접근 제어 — 소유자 · 공개 범위 · 협업자.
--
-- 이 마이그레이션은 정책을 뒤집는다. 지금까지 매크로는 "권한자끼리 전부 공유"였고
-- 작성자는 기록으로만 남았다(0011의 주석 참고). 앞으로는 **작성자가 소유자**이며,
-- 기본값은 비공개다.
--
-- 왜 바꾸는가: 매크로가 늘면 목록은 남의 실험과 반쯤 만든 것으로 뒤덮인다.
-- 버전 관리가 있으니 잘못된 수정은 되돌릴 수 있지만, 되돌릴 수 있다는 것과
-- 남이 건드려도 된다는 것은 다른 이야기다. 대신 **공유는 명시적으로** 한다 —
-- 공개로 열거나, 협업자를 지정한다.
--
-- 판정 사다리는 하나뿐이다(model.MacroAccess):
--   none < view(조회+실행) < edit(+수정) < manage(+공개설정·협업자·자동실행) < own(+삭제)
--   · 작성자        → own
--   · 협업자        → manage   (삭제만 못 한다)
--   · 공개 + edit   → edit
--   · 공개 + view   → view
--   · 슈퍼어드민    → own      (모든 매크로)

-- ---------- 매크로 ----------

-- visibility: private(작성자·협업자만) | public(매크로 권한자 전원)
ALTER TABLE macros ADD COLUMN visibility TEXT NOT NULL DEFAULT 'private';
-- public_access: 공개일 때 남들이 무엇까지 하는가. view(조회+실행) | edit(+수정)
-- 비공개일 때는 읽히지 않는다. 다시 공개로 바꿀 때 직전 선택이 남아 있도록 지우지 않는다.
ALTER TABLE macros ADD COLUMN public_access TEXT NOT NULL DEFAULT 'view';

-- 기존 매크로는 공개+수정 허용으로 옮긴다.
--
-- 새 기본값(비공개)을 소급 적용하지 않는 이유: 지금 도는 것을 깨기 때문이다.
-- 옛 정책에서 만들어진 매크로에는 "이건 나만 볼 것"이라는 의도가 애초에 없었고,
-- 남이 만든 매크로를 매일 돌리던 사람과 그 매크로에 걸린 자동 실행이 있다.
-- 마이그레이션이 그것을 조용히 끊으면 원인을 찾는 데 며칠이 걸린다.
-- 좁히는 것은 소유자가 화면에서 하면 된다 — 넓히는 것과 달리 되돌릴 수 있다.
UPDATE macros SET visibility = 'public', public_access = 'edit';

-- 협업자.
--
-- 협업자는 수정·실행·관리를 하고 삭제만 못 한다. 삭제를 뺀 이유는 삭제가 유일하게
-- 되돌릴 수 없는 동작이어서다 — 버전도 함께 사라진다.
--
-- user_id에 ON DELETE CASCADE를 거는 것은 여기서는 안전하다. 협업자 행이 사라지면
-- 그 사람의 권한이 없어질 뿐, 매크로 자체는 남는다.
CREATE TABLE macro_collaborators (
    macro_id   TEXT NOT NULL REFERENCES macros (id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    added_by   TEXT REFERENCES users (id) ON DELETE SET NULL,
    added_name TEXT NOT NULL DEFAULT '',
    added_at   TEXT NOT NULL,
    PRIMARY KEY (macro_id, user_id)
);

-- 목록 조회는 "내가 협업자인 매크로"를 사용자 쪽에서 찾는다.
-- 기본키가 (macro_id, user_id)라 user_id 단독 조회에는 쓸 수 없다.
CREATE INDEX idx_macro_collab_user ON macro_collaborators (user_id);

-- ---------- 커스텀 노드 ----------
--
-- 노드에도 같은 두 열을 붙이지만, **실제로 읽히는 것은 scope='global' 인 노드뿐이다.**
-- scope='macro' 노드는 소속 매크로의 판정을 그대로 물려받는다(store.NodeDef.Access).
--
-- 물려받게 한 이유: 매크로 전용 노드는 그 매크로의 일부다. 따로 공개 설정을 두면
-- "매크로는 공유했는데 그 안의 노드는 안 보이는" 상태가 만들어지고, 협업자로 부른
-- 사람이 정작 그 매크로의 노드를 못 고치는 모순이 생긴다.
ALTER TABLE macro_node_defs ADD COLUMN visibility TEXT NOT NULL DEFAULT 'private';
ALTER TABLE macro_node_defs ADD COLUMN public_access TEXT NOT NULL DEFAULT 'view';

UPDATE macro_node_defs SET visibility = 'public', public_access = 'edit';

CREATE TABLE macro_node_def_collaborators (
    def_id     TEXT NOT NULL REFERENCES macro_node_defs (id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    added_by   TEXT REFERENCES users (id) ON DELETE SET NULL,
    added_name TEXT NOT NULL DEFAULT '',
    added_at   TEXT NOT NULL,
    PRIMARY KEY (def_id, user_id)
);

CREATE INDEX idx_node_def_collab_user ON macro_node_def_collaborators (user_id);
