package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
)

// Git 연동은 개인의 것이다. 그 경계가 질의 수준에서 지켜지는지 확인한다.
//
// 화면에서 거르는 것으로는 부족하다 — API를 직접 부르면 그만이고, 여기 담긴 것은
// 이 앱 밖(저장소·이슈·CI)에서도 쓸 수 있는 토큰이다.
func vcsFixture(t *testing.T) (context.Context, *Store, string, string) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "vcs.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	alice, err := st.CreateUser(ctx, CreateUserParams{
		Username: "alice", Role: model.RoleMember, PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	// 밥은 슈퍼 어드민이다. 그래도 앨리스의 Git 계정은 볼 수 없어야 한다.
	bob, err := st.CreateUser(ctx, CreateUserParams{
		Username: "bob", Role: model.RoleSuperadmin, PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	return ctx, st, alice.ID, bob.ID
}

func newIntegration(t *testing.T, ctx context.Context, st *Store, owner, name string) *VCSIntegration {
	t.Helper()
	token := "ghp_" + name
	item, err := st.CreateVCSIntegration(ctx, SaveVCSParams{
		Name: name, Provider: "github", Repo: "acme/schema",
		DefaultBranch: "main", BranchTemplate: "schema/{date}", PathTemplate: "m/{ts}",
		Token: &token, Enabled: true, OwnerID: owner,
	})
	if err != nil {
		t.Fatalf("create integration: %v", err)
	}
	return item
}

func TestVCSIntegrationIsPrivateToOwner(t *testing.T) {
	ctx, st, alice, bob := vcsFixture(t)
	item := newIntegration(t, ctx, st, alice, "내 GitHub")

	// 주인은 읽는다.
	if _, err := st.GetVCSIntegration(ctx, item.ID, alice, true); err != nil {
		t.Fatalf("주인이 자기 연동을 읽지 못한다: %v", err)
	}
	// 슈퍼 어드민도 못 읽는다. 존재 여부조차 알려주지 않는다(ErrNotFound).
	if _, err := st.GetVCSIntegration(ctx, item.ID, bob, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("남이 연동을 읽었다: %v", err)
	}

	mine, err := st.ListVCSIntegrations(ctx, alice, "")
	if err != nil || len(mine) != 1 {
		t.Fatalf("내 목록 = %d건 (%v)", len(mine), err)
	}
	others, err := st.ListVCSIntegrations(ctx, bob, "")
	if err != nil {
		t.Fatalf("목록: %v", err)
	}
	if len(others) != 0 {
		t.Errorf("남의 연동이 목록에 보인다: %d건", len(others))
	}
}

func TestVCSIntegrationCannotBeTouchedByOthers(t *testing.T) {
	ctx, st, alice, bob := vcsFixture(t)
	item := newIntegration(t, ctx, st, alice, "내 GitHub")

	// 수정: 남이 부르면 없는 것이 된다.
	_, err := st.UpdateVCSIntegration(ctx, SaveVCSParams{
		ID: item.ID, OwnerID: bob, Name: "가로채기", Provider: "github",
		Repo: "evil/repo", DefaultBranch: "main",
		BranchTemplate: "b", PathTemplate: "p", Enabled: true,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("남이 연동을 수정했다: %v", err)
	}
	// 삭제도 마찬가지.
	if err := st.DeleteVCSIntegration(ctx, item.ID, bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("남이 연동을 삭제했다: %v", err)
	}
	// 원래 값이 그대로여야 한다.
	after, err := st.GetVCSIntegration(ctx, item.ID, alice, false)
	if err != nil {
		t.Fatalf("확인: %v", err)
	}
	if after.Name != "내 GitHub" || after.Repo != "acme/schema" {
		t.Errorf("남의 수정이 반영됐다: %+v", after)
	}
}

// 이름은 사람마다 따로 센다. 같은 이름을 나만 쓸 수 있으면 다른 사람은
// 자기 계정을 등록하면서 남의 이름을 피해 가야 한다.
func TestVCSNameIsUniquePerOwner(t *testing.T) {
	ctx, st, alice, bob := vcsFixture(t)
	newIntegration(t, ctx, st, alice, "회사 저장소")
	newIntegration(t, ctx, st, bob, "회사 저장소")

	token := "ghp_dup"
	_, err := st.CreateVCSIntegration(ctx, SaveVCSParams{
		Name: "회사 저장소", Provider: "github", Repo: "acme/schema",
		DefaultBranch: "main", BranchTemplate: "b", PathTemplate: "p",
		Token: &token, Enabled: true, OwnerID: alice,
	})
	if !errors.Is(err, ErrDuplicateName) {
		t.Errorf("같은 사람이 같은 이름을 두 번 만들었다: %v", err)
	}
}

// 계정이 사라지면 그 사람의 Git 계정도 사라진다.
// 주인 없는 토큰이 DB에 남아 있을 이유가 없다.
func TestVCSIntegrationDiesWithUser(t *testing.T) {
	ctx, st, alice, _ := vcsFixture(t)
	item := newIntegration(t, ctx, st, alice, "내 GitHub")

	if err := st.DeleteUser(ctx, alice); err != nil {
		t.Fatalf("사용자 삭제: %v", err)
	}
	if _, err := st.GetVCSIntegration(ctx, item.ID, alice, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("계정을 지웠는데 Git 연동이 남았다: %v", err)
	}
}

// 주인 없이는 만들 수 없다. 이 검사가 없으면 owner_id가 빈 행이 생기고,
// 그 행은 아무에게도 보이지 않으면서 토큰만 들고 남는다.
func TestVCSIntegrationRequiresOwner(t *testing.T) {
	ctx, st, _, _ := vcsFixture(t)
	token := "ghp_x"
	_, err := st.CreateVCSIntegration(ctx, SaveVCSParams{
		Name: "주인 없음", Provider: "github", Repo: "acme/schema",
		DefaultBranch: "main", BranchTemplate: "b", PathTemplate: "p",
		Token: &token, Enabled: true,
	})
	if err == nil {
		t.Error("주인 없는 연동이 만들어졌다")
	}
}

// 기존 설치에서 이 마이그레이션이 실제로 도는지 확인한다.
//
// 표를 지우고 다시 만드는 마이그레이션이라 위험 지점이 둘 있다: (1) vcs_pushes가
// 이 표를 외래키로 참조하고 있고, (2) 실제 DB에는 행이 들어 있다. 새 DB에서만
// 시험하면 둘 다 만나지 않는다 — 그러면 이 마이그레이션은 검증되지 않은 채
// 남의 운영 DB에서 처음 실행된다.
func TestVCSOwnerMigrationClearsOldRows(t *testing.T) {
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	path := filepath.Join(t.TempDir(), "vcsmig.db")
	db, err := sql.Open("sqlite", strings.ReplaceAll(path, "\\", "/")+
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	st := &Store{db: db, secret: box}
	t.Cleanup(func() { db.Close() })

	if err := st.migrateTo(ctx, 24); err != nil {
		t.Fatalf("migrate to 24: %v", err)
	}

	// 공유 연동 시절의 행. created_by는 등록한 사람일 뿐 토큰 주인이라는 보장이 없다.
	now := nowString()
	if _, err := db.ExecContext(ctx, `INSERT INTO vcs_integrations
		(id, name, provider, base_url, repo, default_branch, branch_template, path_template,
		 username, token_enc, connection_id, enabled, created_by, created_at, updated_at)
		VALUES ('v1','회사 저장소','github','','acme/schema','main','b','p','',
		        'sealed-token', NULL, 1, NULL, ?, ?)`, now, now); err != nil {
		t.Fatalf("insert legacy integration: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO vcs_pushes
		(integration_id, migration_title, branch, status, actor_name, created_at)
		VALUES ('v1','제목','schema/x','ok','누군가',?)`, now); err != nil {
		t.Fatalf("insert legacy push: %v", err)
	}

	if err := st.migrate(ctx); err != nil {
		t.Fatalf("migrate rest: %v", err)
	}

	var integrations, pushes int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM vcs_integrations`).Scan(&integrations); err != nil {
		t.Fatalf("count integrations: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM vcs_pushes`).Scan(&pushes); err != nil {
		t.Fatalf("count pushes: %v", err)
	}
	if integrations != 0 || pushes != 0 {
		t.Errorf("옛 행이 남았다: 연동 %d건, 푸시 %d건", integrations, pushes)
	}

	// 새 스키마가 실제로 쓸 수 있어야 한다 — 주인을 붙여 하나 만들어 본다.
	u, err := st.CreateUser(ctx, CreateUserParams{
		Username: "after", Role: model.RoleMember, PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token := "ghp_new"
	if _, err := st.CreateVCSIntegration(ctx, SaveVCSParams{
		Name: "내 GitHub", Provider: "github", Repo: "acme/schema",
		DefaultBranch: "main", BranchTemplate: "b", PathTemplate: "p",
		Token: &token, Enabled: true, OwnerID: u.ID,
	}); err != nil {
		t.Fatalf("마이그레이션 뒤 연동을 만들 수 없다: %v", err)
	}
}
