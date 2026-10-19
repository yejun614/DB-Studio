package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadSnapshotKeepsChildRows는 스냅샷을 적용해도 자식 행이 살아남는지 본다.
//
// 표마다 비우고 채우기를 번갈아 하던 때는 이름순으로 project_members를 채운 뒤 projects를
// 비우면서 ON DELETE CASCADE가 방금 넣은 참여자를 지웠다. 오류 없이 "프로젝트에 아무도
// 없다"로만 보였다.
func TestLoadSnapshotKeepsChildRows(t *testing.T) {
	ctx, src := clusterFixture(t, "src")
	_, dst := clusterFixture(t, "dst")

	uid := seedUser(t, ctx, src, "alice")
	p, err := src.CreateProject(ctx, SaveProjectParams{Name: "billing", ActorID: uid})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := src.SnapshotTo(ctx, snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := dst.LoadSnapshot(ctx, snap); err != nil {
		t.Fatalf("load: %v", err)
	}
	ok, err := dst.IsProjectMember(ctx, p.ID, uid)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	if !ok {
		t.Fatal("스냅샷을 적용하자 프로젝트 참여자가 사라졌습니다 (CASCADE가 자식 행을 지웠습니다)")
	}
}

// TestCloudRowsRoundTrip은 D1로 보내는 모양(JSON 행)으로 꺼냈다가 넣으면 같아지는지 본다.
// BLOB(아바타)은 16진 문자열로 오가야 하고, 보내지 않는 표(세션)는 받는 쪽 것이 남되
// 사라진 사용자를 가리키는 세션은 정리돼야 한다.
func TestCloudRowsRoundTrip(t *testing.T) {
	ctx, src := clusterFixture(t, "src")
	_, dst := clusterFixture(t, "dst")

	uid := seedUser(t, ctx, src, "alice")
	img := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0x10}
	if _, err := src.SaveUserAvatar(ctx, SaveAvatarParams{
		UserID: uid, Mime: "image/png", Bytes: img, Width: 1, Height: 1, Source: "upload",
	}); err != nil {
		t.Fatalf("avatar: %v", err)
	}
	if _, err := src.CreateSession(ctx, "src-token", uid, time.Hour, "127.0.0.1", "t"); err != nil {
		t.Fatalf("session: %v", err)
	}

	// 받는 쪽에는 다른 사용자와 그 세션이 있다(새 환경의 부트스트랩 관리자).
	other := seedUser(t, ctx, dst, "bootstrap")
	if _, err := dst.CreateSession(ctx, "dst-token", other, time.Hour, "127.0.0.1", "t"); err != nil {
		t.Fatalf("session: %v", err)
	}

	tables, err := src.CloudTables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		if tb == "sessions" || tb == "metric_samples" {
			t.Fatalf("%s 는 D1로 보내지 않아야 합니다", tb)
		}
	}
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := src.SnapshotTo(ctx, snap); err != nil {
		t.Fatal(err)
	}
	type row struct {
		t   string
		rid int64
		raw []byte
	}
	var rows []row
	if _, err := ExportSnapshotRows(ctx, snap, tables, func(tb string, rid int64, raw []byte) error {
		rows = append(rows, row{tb, rid, append([]byte(nil), raw...)})
		return nil
	}); err != nil {
		t.Fatalf("export: %v", err)
	}

	n, err := dst.ReplaceCloudTables(ctx, func(put func(string, int64, []byte) error) error {
		for _, r := range rows {
			if err := put(r.t, r.rid, r.raw); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if n == 0 {
		t.Fatal("넣은 행이 없습니다")
	}

	if _, err := dst.GetUser(ctx, uid); err != nil {
		t.Fatalf("받은 쪽에 사용자가 없습니다: %v", err)
	}
	if _, err := dst.GetUser(ctx, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("받기 전의 사용자가 남아 있습니다: %v", err)
	}
	av, err := dst.GetUserAvatar(ctx, uid, true)
	if err != nil {
		t.Fatalf("avatar: %v", err)
	}
	if !bytes.Equal(av.Bytes, img) {
		t.Fatalf("아바타 바이트가 달라졌습니다: %x, 기대 %x", av.Bytes, img)
	}
	// 사라진 사용자의 세션은 지워진다. 남으면 존재하지 않는 사람의 쿠키가 남는다.
	if _, _, err := dst.LookupSession(ctx, "dst-token"); err == nil {
		t.Fatal("사라진 사용자의 세션이 남아 있습니다")
	}
}

// TestRestoreFromFileKeepsSessions는 복구가 지금 로그인한 세션을 되돌리지 않는지 본다.
func TestRestoreFromFileKeepsSessions(t *testing.T) {
	ctx, st := clusterFixture(t, "restore")
	uid := seedUser(t, ctx, st, "alice")
	snap := filepath.Join(t.TempDir(), "backup.db")
	if err := st.SnapshotTo(ctx, snap); err != nil {
		t.Fatal(err)
	}
	// 백업 뒤에 생긴 것: 사용자 하나와 alice의 세션.
	bob := seedUser(t, ctx, st, "bob")
	if _, err := st.CreateSession(ctx, "alice-token", uid, time.Hour, "127.0.0.1", "t"); err != nil {
		t.Fatal(err)
	}
	if err := st.RestoreFromFile(ctx, snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := st.GetUser(ctx, bob); !errors.Is(err, ErrNotFound) {
		t.Fatalf("백업 뒤에 만든 사용자가 남아 있습니다: %v", err)
	}
	if _, _, err := st.LookupSession(ctx, "alice-token"); err != nil {
		t.Fatalf("복구가 지금의 세션을 지웠습니다: %v", err)
	}
}

func TestLocalStateIsNotReplicated(t *testing.T) {
	ctx, st := clusterFixture(t, "state")
	id, err := st.InstanceID(ctx)
	if err != nil || id == "" {
		t.Fatalf("instance id: %q %v", id, err)
	}
	again, _ := st.InstanceID(ctx)
	if again != id {
		t.Fatalf("설치본 ID가 바뀌었습니다: %s → %s", id, again)
	}
	tables, _ := st.ReplTables(ctx)
	for _, tb := range tables {
		if tb == "repl_state" {
			t.Fatal("repl_state 가 복제 대상에 들어 있습니다")
		}
	}
}
