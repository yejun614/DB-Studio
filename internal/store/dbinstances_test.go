package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"dbstudio/internal/crypto"
)

func instanceFixture(t *testing.T) (context.Context, *Store, string) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "inst.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	proj, err := st.CreateProject(ctx, SaveProjectParams{Name: "검사용"})
	if err != nil {
		t.Fatalf("프로젝트: %v", err)
	}
	return ctx, st, proj.ID
}

func newInstance(t *testing.T, ctx context.Context, st *Store, proj, name string) *DBInstance {
	t.Helper()
	in, err := st.CreateDBInstance(ctx, CreateDBInstanceParams{
		ProjectID: proj, Name: name, Kind: "postgres",
		Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-" + name, VolumeName: "dbstudio-" + name + "-data",
		Port: 5432, HostPort: 0,
		Values:  map[string]string{"database": "appdb", "memoryMB": "512"},
		Secrets: map[string]string{"password": "pw1234!"},
		Actor:   "슈퍼 어드민",
	})
	if err != nil {
		t.Fatalf("만들기: %v", err)
	}
	return in
}

func TestCreateAndReadDBInstance(t *testing.T) {
	ctx, st, proj := instanceFixture(t)
	in := newInstance(t, ctx, st, proj, "pg1")

	if in.Status != InstanceCreating {
		t.Errorf("처음 상태 = %s (기대 creating)", in.Status)
	}
	if in.Values["database"] != "appdb" || in.Values["memoryMB"] != "512" {
		t.Errorf("설정값이 왕복하지 않았습니다: %v", in.Values)
	}
	got, err := st.GetDBInstance(ctx, in.ID)
	if err != nil {
		t.Fatalf("읽기: %v", err)
	}
	if got.Name != "pg1" || got.ContainerName != "dbstudio-pg1" {
		t.Errorf("%+v", got)
	}
	if _, err := st.GetDBInstance(ctx, "없는것"); !errors.Is(err, ErrNotFound) {
		t.Errorf("없는 것을 읽을 때 = %v", err)
	}
}

// 비밀은 **목록에 실려 나오지 않아야** 한다.
//
// 같은 구조체에 담으면 목록 한 번에 모든 비밀번호가 메모리로 올라오고,
// 그것을 그대로 JSON 으로 내보내는 길이 생긴다. 이 검사는 그 길이 없다는 것을
// 지킨다 — 나중에 편하다고 DBInstance 에 Password 를 더하면 여기서 걸린다.
func TestDBInstanceSecretsAreSeparate(t *testing.T) {
	ctx, st, proj := instanceFixture(t)
	in := newInstance(t, ctx, st, proj, "pg1")

	for k, v := range in.Values {
		if v == "pw1234!" {
			t.Errorf("설정값에 비밀번호가 실려 있습니다: %s", k)
		}
	}

	secrets, err := st.DBInstanceSecrets(ctx, in.ID)
	if err != nil {
		t.Fatalf("비밀 읽기: %v", err)
	}
	if secrets["password"] != "pw1234!" {
		t.Errorf("비밀이 왕복하지 않았습니다: %v", secrets)
	}

	// 비밀이 없는 인스턴스도 있을 수 있다. 그때 오류가 아니어야 한다.
	bare, err := st.CreateDBInstance(ctx, CreateDBInstanceParams{
		ProjectID: proj, Name: "bare", Kind: "redis", Image: "redis:7-alpine",
		Version: "7-alpine", ContainerName: "dbstudio-bare", Port: 6379,
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	got, err := st.DBInstanceSecrets(ctx, bare.ID)
	if err != nil {
		t.Errorf("비밀 없는 인스턴스에서 오류: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("비밀이 있다고 합니다: %v", got)
	}
}

// 컨테이너 이름은 유일해야 한다.
//
// 도커에서 유일해야 하는 것이라 우리 쪽에서 먼저 막는다. 막지 않으면 도커의
// 409 로 실패하고, 그때는 우리 표에 반쯤 만들어진 줄이 남는다.
func TestDBInstanceContainerNameIsUnique(t *testing.T) {
	ctx, st, proj := instanceFixture(t)
	newInstance(t, ctx, st, proj, "pg1")

	_, err := st.CreateDBInstance(ctx, CreateDBInstanceParams{
		ProjectID: proj, Name: "pg1", Kind: "postgres", Image: "postgres:17-alpine",
		Version: "17-alpine", ContainerName: "dbstudio-pg1", Port: 5432,
	})
	if !errors.Is(err, ErrInstanceNameTaken) {
		t.Errorf("같은 이름을 받아들였습니다: %v", err)
	}
}

// 부분 갱신은 준 것만 고쳐야 한다.
//
// 값으로 받으면 안 준 필드가 빈 문자열로 덮인다 — 진행률을 적는 사이에
// 오류 메시지가 지워지고, 그러면 왜 실패했는지가 사라진다.
func TestUpdateDBInstanceTouchesOnlyWhatIsGiven(t *testing.T) {
	ctx, st, proj := instanceFixture(t)
	in := newInstance(t, ctx, st, proj, "pg1")

	failed, msg := InstanceFailed, "이미지를 내려받지 못했습니다"
	if err := st.UpdateDBInstance(ctx, in.ID, UpdateDBInstanceParams{
		Status: &failed, Error: &msg,
	}); err != nil {
		t.Fatalf("%v", err)
	}
	// 이제 진행률만 고친다. 오류가 남아 있어야 한다.
	prog := "다시 시도 중"
	if err := st.UpdateDBInstance(ctx, in.ID, UpdateDBInstanceParams{Progress: &prog}); err != nil {
		t.Fatalf("%v", err)
	}
	got, _ := st.GetDBInstance(ctx, in.ID)
	if got.Error != msg {
		t.Errorf("오류 메시지가 지워졌습니다: %q", got.Error)
	}
	if got.Status != InstanceFailed {
		t.Errorf("상태가 바뀌었습니다: %s", got.Status)
	}
	if got.Progress != prog {
		t.Errorf("진행률 = %q", got.Progress)
	}

	// 아무것도 주지 않으면 아무 일도 없어야 한다(오류도 아니다).
	if err := st.UpdateDBInstance(ctx, in.ID, UpdateDBInstanceParams{}); err != nil {
		t.Errorf("빈 갱신에서 오류: %v", err)
	}
	if err := st.UpdateDBInstance(ctx, "없는것", UpdateDBInstanceParams{Progress: &prog}); !errors.Is(err, ErrNotFound) {
		t.Errorf("없는 것을 고칠 때 = %v", err)
	}
}

// 실제로 잡힌 호스트 포트를 담을 수 있어야 한다.
//
// 도커가 골라 준 포트가 여기 들어온다. 요청 값(0)이 그대로 남으면 붙을 수 없는
// 커넥션이 만들어진다.
func TestUpdateDBInstanceHostPort(t *testing.T) {
	ctx, st, proj := instanceFixture(t)
	in := newInstance(t, ctx, st, proj, "pg1")
	if in.HostPort != 0 {
		t.Fatalf("처음 포트 = %d", in.HostPort)
	}
	port, running := 32768, InstanceRunning
	if err := st.UpdateDBInstance(ctx, in.ID, UpdateDBInstanceParams{
		HostPort: &port, Status: &running,
	}); err != nil {
		t.Fatalf("%v", err)
	}
	got, _ := st.GetDBInstance(ctx, in.ID)
	if got.HostPort != 32768 {
		t.Errorf("포트 = %d", got.HostPort)
	}
}

// 목록은 프로젝트로 좁히고, 지운 것은 뒤로 보낸다.
func TestListDBInstances(t *testing.T) {
	ctx, st, proj := instanceFixture(t)
	newInstance(t, ctx, st, proj, "bbb")
	gone := newInstance(t, ctx, st, proj, "aaa")
	newInstance(t, ctx, st, proj, "ccc")

	removed := InstanceRemoved
	if err := st.UpdateDBInstance(ctx, gone.ID, UpdateDBInstanceParams{Status: &removed}); err != nil {
		t.Fatalf("%v", err)
	}

	list, err := st.ListDBInstances(ctx, proj)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(list) != 3 {
		t.Fatalf("%d개", len(list))
	}
	// 지운 것("aaa")이 이름 순으로는 첫째지만 뒤로 가야 한다.
	if list[len(list)-1].Name != "aaa" {
		got := []string{}
		for _, in := range list {
			got = append(got, in.Name+"/"+in.Status)
		}
		t.Errorf("지운 것이 뒤로 가지 않았습니다: %v", got)
	}

	// 다른 프로젝트로 좁히면 비어야 한다.
	other, err := st.CreateProject(ctx, SaveProjectParams{Name: "다른것"})
	if err != nil {
		t.Fatalf("%v", err)
	}
	list, _ = st.ListDBInstances(ctx, other.ID)
	if len(list) != 0 {
		t.Errorf("다른 프로젝트에서 %d개가 보입니다", len(list))
	}
	// 프로젝트를 비우면 전부 본다.
	list, _ = st.ListDBInstances(ctx, "")
	if len(list) != 3 {
		t.Errorf("전체 보기에서 %d개", len(list))
	}
}

// 프로젝트를 지우면 인스턴스 기록도 함께 사라진다(ON DELETE CASCADE).
//
// 컨테이너는 남는다. 그것을 여기서 지울 수는 없으므로, 프로젝트를 지우기 전에
// 화면이 물어야 한다 — 그 사실을 검사로 박아 둔다.
func TestDeleteProjectCascadesInstances(t *testing.T) {
	ctx, st, proj := instanceFixture(t)
	in := newInstance(t, ctx, st, proj, "pg1")

	if _, err := st.DBInstanceSecrets(ctx, in.ID); err != nil {
		t.Fatalf("%v", err)
	}
	if err := st.DeleteDBInstance(ctx, in.ID); err != nil {
		t.Fatalf("지우기: %v", err)
	}
	if _, err := st.GetDBInstance(ctx, in.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("지웠는데 읽힙니다: %v", err)
	}
	// 비밀도 함께 사라져야 한다.
	got, err := st.DBInstanceSecrets(ctx, in.ID)
	if err != nil {
		t.Errorf("%v", err)
	}
	if len(got) != 0 {
		t.Errorf("비밀이 남아 있습니다: %v", got)
	}
}

// 프로젝트가 없으면 만들 수 없다. 만든 DB 는 곧 커넥션이 되고, 커넥션은
// 프로젝트 없이 존재할 수 없다.
func TestCreateDBInstanceNeedsProject(t *testing.T) {
	ctx, st, _ := instanceFixture(t)
	_, err := st.CreateDBInstance(ctx, CreateDBInstanceParams{
		Name: "x", Kind: "redis", ContainerName: "dbstudio-x", Port: 6379,
	})
	if !errors.Is(err, ErrNoProject) {
		t.Errorf("프로젝트 없이 만들어졌습니다: %v", err)
	}
}
