package cloud

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// fakeD1은 D1 REST API 를 흉내 낸다. 문장은 진짜 SQLite 에서 돈다 — 그래야 우리가 보내는
// SQL(INSERT … SELECT … ON CONFLICT, 주인 조건)이 실제로 받아들여지는지까지 시험한다.
type fakeD1 struct {
	t        *testing.T
	db       *sql.DB
	mu       sync.Mutex
	requests int
	fail     error
}

func newFakeD1(t *testing.T) *fakeD1 {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(t.TempDir(), "d1.db")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	f := &fakeD1{t: t, db: db}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	prev := d1BaseURL
	d1BaseURL = srv.URL
	t.Cleanup(func() { d1BaseURL = prev })
	return f
}

func (f *fakeD1) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"success": false,
			"errors": []map[string]any{{"code": 10000, "message": "Authentication error"}}})
		return
	}
	body, _ := io.ReadAll(r.Body)
	var in struct {
		Stmt
		Batch []Stmt `json:"batch"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		f.t.Errorf("요청 본문: %v", err)
	}
	stmts := in.Batch
	if len(stmts) == 0 {
		stmts = []Stmt{in.Stmt}
	}
	fail := func(err error) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"success": false,
			"errors": []map[string]any{{"code": 7500, "message": err.Error()}}})
	}
	if f.fail != nil {
		fail(f.fail)
		return
	}
	// D1 의 batch 는 한 트랜잭션이다.
	tx, err := f.db.Begin()
	if err != nil {
		fail(err)
		return
	}
	var results []map[string]any
	for _, st := range stmts {
		args := make([]any, len(st.Params))
		for i, p := range st.Params {
			args[i] = p
		}
		rows, err := tx.Query(st.SQL, args...)
		if err != nil {
			tx.Rollback()
			fail(err)
			return
		}
		cols, _ := rows.Columns()
		out := []map[string]any{}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			m := map[string]any{}
			for i, c := range cols {
				if b, ok := vals[i].([]byte); ok {
					vals[i] = string(b)
				}
				m[c] = vals[i]
			}
			out = append(out, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			tx.Rollback()
			fail(err)
			return
		}
		results = append(results, map[string]any{"results": out, "success": true})
	}
	if err := tx.Commit(); err != nil {
		fail(err)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "result": results, "errors": []any{}})
}

func (f *fakeD1) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	if err := f.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (f *fakeD1) meta(t *testing.T, k string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var v string
	_ = f.db.QueryRow(`SELECT v FROM dbstudio_meta WHERE k = ?`, k).Scan(&v)
	return v
}

// env는 DB Studio 설치본 하나다(메타 DB + 서비스).
type env struct {
	st  *store.Store
	svc *Service
	ctx context.Context
	// replaced는 OnReplaced 가 불린 횟수다.
	replaced int
}

func newEnv(t *testing.T, name string, key byte) *env {
	t.Helper()
	ctx := context.Background()
	k := bytes.Repeat([]byte{key}, 32)
	box, err := crypto.NewSecretBox(k)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "meta.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, ctx: ctx}
	e.svc = New(st, Options{DataDir: dir, NodeName: name, OnReplaced: func(string) { e.replaced++ }})
	return e
}

func (e *env) connectD1(t *testing.T) {
	t.Helper()
	if err := e.st.SaveD1Settings(e.ctx, store.D1Settings{
		Enabled: true, AccountID: "acct", DatabaseID: "db", APIToken: "tok", IntervalSec: 10,
	}, ""); err != nil {
		t.Fatal(err)
	}
}

func (e *env) user(t *testing.T, name string) string {
	t.Helper()
	u, err := e.st.CreateUser(e.ctx, store.CreateUserParams{
		Username: name, DisplayName: name, Role: model.RoleMember, PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// sync는 정기 동기화 한 번을 그대로 돈다(주기는 건너뛴다).
func (e *env) sync(t *testing.T) (int, error) {
	t.Helper()
	e.svc.opMu.Lock()
	defer e.svc.opMu.Unlock()
	return e.svc.syncOnce(e.ctx)
}

func waitJob(t *testing.T, s *Service) *Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		running, last := s.job, s.lastJob
		s.mu.Unlock()
		if running == nil && last != nil {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("작업이 끝나지 않았습니다")
	return nil
}

// TestD1PushThenTakeOver는 이 기능의 약속 전체를 따라간다.
//
// 서버 A가 D1 에 올리고 → 변경을 이어 보내고 → 새 환경 B가 같은 D1 을 연결해 받아 오면
// A의 데이터로 운영되고 → 주인이 B로 바뀌어 A는 더 이상 덮어쓰지 못한다.
func TestD1PushThenTakeOver(t *testing.T) {
	d1 := newFakeD1(t)
	a := newEnv(t, "server-a", 7)
	a.connectD1(t)
	alice := a.user(t, "alice")

	// 처음 연결: 원격이 비어 있으므로 묻지 않고 전체를 올린다.
	if _, err := a.sync(t); err != nil {
		t.Fatalf("첫 동기화: %v", err)
	}
	if got := d1.meta(t, "state"); got != "ready" {
		t.Fatalf("원격 상태 %q, 기대 ready", got)
	}
	if n := d1.count(t, `SELECT count(*) FROM dbstudio_rows WHERE tbl = 'users'`); n != 1 {
		t.Fatalf("원격 사용자 %d명, 기대 1", n)
	}
	if n := d1.count(t, `SELECT count(*) FROM dbstudio_rows WHERE tbl = 'sessions'`); n != 0 {
		t.Fatal("세션은 올리지 않아야 합니다")
	}

	// 이어서 바뀐 것만 보낸다. 같은 행을 여러 번 바꿔도 마지막 상태 하나로 간다.
	bob := a.user(t, "bob")
	for i := 0; i < 5; i++ {
		if _, err := a.st.UpdateUser(a.ctx, bob, store.UpdateUserParams{DisplayName: strptr("Bob " + string(rune('A'+i)))}); err != nil {
			t.Fatal(err)
		}
	}
	before := d1.requests
	sent, err := a.sync(t)
	if err != nil {
		t.Fatalf("변경 동기화: %v", err)
	}
	if d1.requests-before != 1 {
		t.Errorf("변경을 %d번의 요청으로 보냈습니다. 한 번에 모아 보내야 합니다", d1.requests-before)
	}
	if sent > 3 {
		t.Errorf("행 %d개를 보냈습니다 — 같은 행의 변경은 마지막 것 하나만 가야 합니다", sent)
	}
	if n := d1.count(t, `SELECT count(*) FROM dbstudio_rows WHERE tbl = 'users' AND row LIKE '%Bob E%'`); n != 1 {
		t.Fatal("마지막으로 바꾼 이름이 원격에 없습니다")
	}
	// 보낸 로그는 지운다. 남기면 단일 서버의 메타 DB가 끝없이 커진다.
	if lo, hi, _ := a.st.ReplBounds(a.ctx); hi != 0 && lo != 0 {
		t.Errorf("보낸 변경 로그가 남아 있습니다 (%d..%d)", lo, hi)
	}

	// 지운 것도 지운다.
	if err := a.st.DeleteUser(a.ctx, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := a.sync(t); err != nil {
		t.Fatal(err)
	}
	if n := d1.count(t, `SELECT count(*) FROM dbstudio_rows WHERE tbl = 'users'`); n != 1 {
		t.Fatalf("지운 사용자가 원격에 남아 있습니다 (%d명)", n)
	}

	// 새 환경 B: 같은 마스터 키, 자기만의 부트스트랩 사용자가 있다.
	b := newEnv(t, "server-b", 7)
	boot := b.user(t, "superadmin")
	b.connectD1(t)
	if _, err := b.sync(t); err != nil {
		t.Fatalf("B 동기화: %v", err)
	}
	b.svc.mu.Lock()
	state := b.svc.d1.State
	b.svc.mu.Unlock()
	if state != "setup" {
		t.Fatalf("남의 데이터가 있는 D1 을 B가 말없이 다뤘습니다 (상태 %q)", state)
	}
	if n := d1.count(t, `SELECT count(*) FROM dbstudio_rows WHERE tbl = 'users'`); n != 1 {
		t.Fatal("B가 묻지 않고 원격을 덮어썼습니다")
	}

	if _, err := b.svc.StartPull(nil); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, b.svc); j.Error != "" || !j.Relogin {
		t.Fatalf("가져오기: %+v", j)
	}
	if _, err := b.st.GetUser(b.ctx, alice); err != nil {
		t.Fatalf("B에 A의 사용자가 없습니다: %v", err)
	}
	if _, err := b.st.GetUser(b.ctx, boot); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("B의 부트스트랩 사용자가 남아 있습니다 — 받아 온 데이터로 운영해야 합니다")
	}
	if b.replaced != 1 {
		t.Errorf("메타 DB를 바꾼 뒤 알림이 %d번 갔습니다", b.replaced)
	}
	// D1 설정은 받아 온 것에 덮이지 않는다.
	if cfg, _ := b.st.D1Settings(b.ctx); !cfg.Enabled || cfg.APIToken != "tok" {
		t.Fatalf("가져오기가 D1 연결 설정을 덮었습니다: %+v", cfg)
	}
	if d1.meta(t, "owner_name") != "server-b" {
		t.Fatal("가져온 B가 D1 의 주인이 되지 않았습니다")
	}

	// B의 변경은 이어서 올라간다.
	b.user(t, "carol")
	if _, err := b.sync(t); err != nil {
		t.Fatalf("B 변경 동기화: %v", err)
	}
	if n := d1.count(t, `SELECT count(*) FROM dbstudio_rows WHERE tbl = 'users'`); n != 2 {
		t.Fatalf("B의 새 사용자가 원격에 없습니다 (%d명)", n)
	}

	// A는 이제 주인이 아니다. 쓰기는 아무 일도 하지 않고, A는 그 사실을 안다.
	a.user(t, "mallory")
	if _, err := a.sync(t); !errors.Is(err, errNotOwner) {
		t.Fatalf("주인이 아닌 A의 동기화: %v, 기대 errNotOwner", err)
	}
	if n := d1.count(t, `SELECT count(*) FROM dbstudio_rows WHERE row LIKE '%mallory%'`); n != 0 {
		t.Fatal("주인이 아닌 A가 원격에 썼습니다")
	}
}

func strptr(s string) *string { return &s }

// TestD1PullRefusesOtherKey는 다른 마스터 키로 봉인된 데이터를 받지 않는지 본다.
func TestD1PullRefusesOtherKey(t *testing.T) {
	newFakeD1(t)
	a := newEnv(t, "a", 1)
	a.connectD1(t)
	a.user(t, "alice")
	if _, err := a.sync(t); err != nil {
		t.Fatal(err)
	}
	b := newEnv(t, "b", 2)
	b.connectD1(t)
	if _, err := b.svc.StartPull(nil); err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, b.svc)
	if !strings.Contains(j.Error, "마스터 키") {
		t.Fatalf("다른 키의 데이터를 받았습니다: %+v", j)
	}
}

// TestD1Disable는 동기화를 끄면 변경 추적을 걷어내는지 본다.
func TestD1Disable(t *testing.T) {
	newFakeD1(t)
	a := newEnv(t, "a", 1)
	a.connectD1(t)
	if _, err := a.sync(t); err != nil {
		t.Fatal(err)
	}
	cfg, _ := a.st.D1Settings(a.ctx)
	cfg.Enabled = false
	if err := a.st.SaveD1Settings(a.ctx, *cfg, ""); err != nil {
		t.Fatal(err)
	}
	a.svc.tickD1(a.ctx)
	a.user(t, "after")
	if _, hi, _ := a.st.ReplBounds(a.ctx); hi != 0 {
		t.Fatal("동기화를 껐는데 변경이 계속 기록됩니다")
	}
}

// fakeS3는 버킷 하나를 메모리에 둔다. 서명은 있는지만 본다(서명 계산은 storage 시험이 지킨다).
type fakeS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
	meta map[string]http.Header
}

func newFakeS3(t *testing.T) (*fakeS3, string) {
	f := &fakeS3{objs: map[string][]byte{}, meta: map[string]http.Header{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	switch {
	case r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objs[key] = b
		f.meta[key] = r.Header.Clone()
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		prefix := r.URL.Query().Get("prefix")
		keys := []string{}
		for k := range f.objs {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString(`<ListBucketResult><IsTruncated>false</IsTruncated>`)
		for _, k := range keys {
			b.WriteString(`<Contents><Key>` + k + `</Key><Size>1</Size><LastModified>2026-10-19T00:00:00Z</LastModified></Contents>`)
		}
		b.WriteString(`</ListBucketResult>`)
		w.Write([]byte(b.String()))
	case r.Method == http.MethodGet:
		b, ok := f.objs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
			return
		}
		for k, v := range f.meta[key] {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
				w.Header()[k] = v
			}
		}
		w.Write(b)
	case r.Method == http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (e *env) connectBucket(t *testing.T, endpoint string, keep int) {
	t.Helper()
	if err := e.st.SaveObjectSettings(e.ctx, store.ObjectSettings{
		Provider: "custom", Endpoint: endpoint, Bucket: "bucket", Prefix: "dbstudio",
		AccessKey: "ak", SecretKey: "sk", Encrypt: true, Keep: keep,
	}, ""); err != nil {
		t.Fatal(err)
	}
}

// TestObjectBackupRestore는 올린 백업으로 되돌리면 그 시점으로 돌아가는지 본다.
func TestObjectBackupRestore(t *testing.T) {
	s3, endpoint := newFakeS3(t)
	a := newEnv(t, "a", 3)
	a.connectBucket(t, endpoint, 2)
	alice := a.user(t, "alice")

	if _, err := a.svc.StartBackup(); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a.svc); j.Error != "" {
		t.Fatalf("백업: %s", j.Error)
	}
	list, err := a.svc.ListBackups(a.ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("백업 목록: %v %v", list, err)
	}
	if !list[0].Encrypted || !strings.HasPrefix(list[0].Key, "dbstudio/") {
		t.Fatalf("백업 이름 %q (암호화 %v)", list[0].Key, list[0].Encrypted)
	}
	// 올라간 것은 평문 SQLite 도, 평문 gzip 도 아니어야 한다.
	raw := s3.objs[list[0].Key]
	if bytes.Contains(raw, []byte("SQLite format")) || bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
		t.Fatal("백업이 암호화되지 않은 채 올라갔습니다")
	}

	bob := a.user(t, "bob")
	if _, err := a.svc.StartRestore(list[0].Key, nil); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a.svc); j.Error != "" || !j.Relogin {
		t.Fatalf("복구: %+v", j)
	}
	if _, err := a.st.GetUser(a.ctx, alice); err != nil {
		t.Fatalf("백업에 있던 사용자가 없습니다: %v", err)
	}
	if _, err := a.st.GetUser(a.ctx, bob); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("백업 뒤에 만든 사용자가 남아 있습니다")
	}
	if cfg, _ := a.st.ObjectSettings(a.ctx); cfg.SecretKey != "sk" {
		t.Fatal("복구가 스토리지 연결 설정을 덮었습니다")
	}

	// 보존 개수(2)를 넘으면 오래된 것부터 지운다. 이름이 초 단위라 겹치지 않게 기다린다.
	for i := 0; i < 2; i++ {
		time.Sleep(1100 * time.Millisecond)
		if _, err := a.svc.StartBackup(); err != nil {
			t.Fatal(err)
		}
		if j := waitJob(t, a.svc); j.Error != "" {
			t.Fatal(j.Error)
		}
	}
	// 이 앱이 만들지 않은 객체는 건드리지 않는다.
	s3.objs["dbstudio/notes.txt"] = []byte("keep me")
	time.Sleep(1100 * time.Millisecond)
	if _, err := a.svc.StartBackup(); err != nil {
		t.Fatal(err)
	}
	waitJob(t, a.svc)
	list, _ = a.svc.ListBackups(a.ctx)
	if len(list) != 2 {
		t.Fatalf("보존 개수를 넘은 백업이 남았습니다: %d개", len(list))
	}
	if _, ok := s3.objs["dbstudio/notes.txt"]; !ok {
		t.Fatal("이 앱의 백업이 아닌 객체를 지웠습니다")
	}
}

// TestRestoreRefusesOtherKey는 다른 키의 서버가 만든 백업을 거절하는지 본다.
func TestRestoreRefusesOtherKey(t *testing.T) {
	_, endpoint := newFakeS3(t)
	a := newEnv(t, "a", 3)
	a.connectBucket(t, endpoint, 0)
	a.user(t, "alice")
	a.svc.StartBackup()
	waitJob(t, a.svc)
	list, _ := a.svc.ListBackups(a.ctx)

	b := newEnv(t, "b", 4)
	b.connectBucket(t, endpoint, 0)
	if _, err := b.svc.StartRestore(list[0].Key, nil); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, b.svc); !strings.Contains(j.Error, "마스터 키") {
		t.Fatalf("다른 키의 백업을 받았습니다: %+v", j)
	}
}

func TestSealRoundTripAndTruncation(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	plain := bytes.Repeat([]byte("dbstudio "), 20000) // 조각 여러 개
	var buf bytes.Buffer
	w, err := NewSealWriter(&buf, key)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(plain)
	w.Close()

	r, err := NewOpenReader(bytes.NewReader(buf.Bytes()), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("되돌린 내용이 다릅니다: %v", err)
	}

	// 마지막 조각을 잘라 낸 파일은 오류여야 한다. 조용히 짧은 백업으로 읽히면 안 된다.
	cut := buf.Bytes()[:buf.Len()-40]
	r, _ = NewOpenReader(bytes.NewReader(cut), key)
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("잘린 백업을 오류 없이 읽었습니다")
	}
	// 다른 키로는 열리지 않는다.
	r, _ = NewOpenReader(bytes.NewReader(buf.Bytes()), bytes.Repeat([]byte{8}, 32))
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("다른 키로 백업이 열렸습니다")
	}
}

func TestNextRun(t *testing.T) {
	loc := time.FixedZone("KST", 9*3600)
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, loc)
	cases := []struct {
		at    string
		every int
		last  time.Time
		want  time.Time
	}{
		// 처음: 오늘 03:00은 지났으니 내일 03:00
		{"03:00", 24, time.Time{}, time.Date(2026, 10, 20, 3, 0, 0, 0, loc)},
		// 6시간마다, 기준 03:00 → 09:00, 15:00 … 지금(10:00) 뒤의 첫 시각
		{"03:00", 6, time.Time{}, time.Date(2026, 10, 19, 15, 0, 0, 0, loc)},
		// 어제 03:00에 마지막으로 했고 서버가 꺼져 있었다 → 오늘 03:00(과거) — 곧바로 따라잡는다
		{"03:00", 24, time.Date(2026, 10, 18, 3, 0, 5, 0, loc), time.Date(2026, 10, 19, 3, 0, 0, 0, loc)},
		// 기준 시각이 마지막 시도보다 뒤
		{"23:30", 24, time.Date(2026, 10, 19, 9, 0, 0, 0, loc), time.Date(2026, 10, 19, 23, 30, 0, 0, loc)},
	}
	for _, c := range cases {
		got := NextRun(c.at, c.every, c.last, now)
		if !got.Equal(c.want) {
			t.Errorf("NextRun(%s, %d, %v) = %v, 기대 %v", c.at, c.every, c.last, got, c.want)
		}
	}
}
