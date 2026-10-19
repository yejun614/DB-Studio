package cloud

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/store"
)

// D1 동기화.
//
// 변경 추적은 클러스터 복제와 같은 장치를 쓴다: 메타 DB의 표에 트리거를 달아 행 변경을
// repl_log에 적는다. 주기마다 그 로그를 읽어 **같은 행의 변경은 마지막 것만 남기고**(JPA가
// flush 때 바뀐 엔티티의 최종 상태만 쓰는 것과 같다) 한 번의 batch 요청으로 보낸다.
//
// D1에는 메타 DB의 표를 그대로 만들지 않는다. 두 표만 둔다.
//
//	dbstudio_rows (tbl, rid, row, updated_at)  — 행 하나가 한 줄. row는 복제 로그와 같은 JSON
//	dbstudio_meta (k, v)                       — 주인·형식·스키마 버전·어디까지 보냈는가
//
// 표를 그대로 옮기지 않는 이유: 이 앱은 버전마다 메타 DB 스키마가 바뀐다(마이그레이션 44개).
// 표를 그대로 두면 바이너리를 올릴 때마다 D1에도 같은 ALTER를 해야 하고, 그 순서가 한 번만
// 어긋나도 동기화가 멈춘다. 행을 JSON으로 두면 원격은 스키마를 모르고, 받는 쪽 바이너리가
// 자기 스키마에 맞춰 넣는다(없는 컬럼은 건너뛰고, 새 컬럼은 기본값) — 클러스터가 버전이 다른
// 노드 사이에서 쓰는 규칙과 같다. BLOB(아바타)도 16진 문자열로 실려 JSON에 담긴다.

const (
	remoteFormat = "1"
	// 한 요청에 담는 문장 수와 크기. D1 문장 하나의 상한(100KB)은 문장 텍스트 기준이고
	// 매개변수는 따로다. 요청 하나가 너무 크면 실패했을 때 다시 보낼 양도 커진다.
	maxBatchStmts = 100
	maxBatchBytes = 1 << 20
	// pullPage는 가져오기에서 한 번에 읽는 행 수다.
	pullPage = 500
	// maxPendingLog를 넘게 밀리면 로그를 버리고 전체를 다시 올린다. D1에 오래 닿지 못한
	// 사이 로그가 끝없이 쌓여 메타 DB를 키우는 것을 막는다.
	maxPendingLog = 200000
)

var remoteSchema = []Stmt{
	{SQL: `CREATE TABLE IF NOT EXISTS dbstudio_rows (
		tbl TEXT NOT NULL,
		rid INTEGER NOT NULL,
		row TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (tbl, rid)
	)`},
	{SQL: `CREATE TABLE IF NOT EXISTS dbstudio_meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`},
}

// ownerGuard는 "아직 내가 주인일 때만"이다. 쓰는 문장마다 붙인다.
//
// 요청 전에 주인을 한 번 확인하는 것으로는 부족하다: 확인과 쓰기 사이에 다른 환경이
// 가져오기를 마치고 주인이 될 수 있다. 문장마다 조건을 걸면, 주인이 바뀐 뒤의 쓰기는
// 아무 일도 하지 않고 지나간다 — 새 주인이 받아 간 데이터를 옛 주인이 덮어쓰지 못한다.
const ownerGuard = `EXISTS (SELECT 1 FROM dbstudio_meta WHERE k = 'owner' AND v = ?)`

func upsertRow(tbl string, rid int64, row, at, owner string) Stmt {
	return Stmt{
		SQL: `INSERT INTO dbstudio_rows (tbl, rid, row, updated_at) SELECT ?, ?, ?, ? WHERE ` + ownerGuard +
			` ON CONFLICT (tbl, rid) DO UPDATE SET row = excluded.row, updated_at = excluded.updated_at`,
		Params: []string{tbl, strconv.FormatInt(rid, 10), row, at, owner},
	}
}

func deleteRow(tbl string, rid int64, owner string) Stmt {
	return Stmt{
		SQL:    `DELETE FROM dbstudio_rows WHERE tbl = ? AND rid = ? AND ` + ownerGuard,
		Params: []string{tbl, strconv.FormatInt(rid, 10), owner},
	}
}

func setMeta(k, v, owner string) Stmt {
	return Stmt{
		SQL: `INSERT INTO dbstudio_meta (k, v) SELECT ?, ? WHERE ` + ownerGuard +
			` ON CONFLICT (k) DO UPDATE SET v = excluded.v`,
		Params: []string{k, v, owner},
	}
}

// deleteTables는 tables의 행을 원격에서 모두 지운다. 보내지 않기로 한 표를 치울 때 쓴다.
func deleteTables(tables []string, owner string) Stmt {
	holes := strings.TrimSuffix(strings.Repeat("?, ", len(tables)), ", ")
	return Stmt{
		SQL:    `DELETE FROM dbstudio_rows WHERE tbl IN (` + holes + `) AND ` + ownerGuard,
		Params: append(append([]string{}, tables...), owner),
	}
}

// claimMeta는 주인 조건 없이 쓴다. 사람이 "이 환경을 주인으로" 고른 경우에만 쓴다.
func claimMeta(k, v string) Stmt {
	return Stmt{
		SQL:    `INSERT INTO dbstudio_meta (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`,
		Params: []string{k, v},
	}
}

var ownerQuery = Stmt{SQL: `SELECT v FROM dbstudio_meta WHERE k = 'owner'`}

// RemoteInfo는 원격 D1에 무엇이 있는지다. 연결 확인과 "가져올까, 덮어쓸까"를 고를 때 보인다.
type RemoteInfo struct {
	// Exists가 거짓이면 아직 DB Studio가 쓴 적 없는 빈 D1이다.
	Exists        bool   `json:"exists"`
	State         string `json:"state,omitempty"` // pushing | ready
	Owner         string `json:"owner,omitempty"`
	OwnerName     string `json:"ownerName,omitempty"`
	Mine          bool   `json:"mine"`
	Rows          int64  `json:"rows"`
	SchemaVersion int    `json:"schemaVersion"`
	LocalSchema   int    `json:"localSchema"`
	// KeyMatches는 원격 데이터가 이 서버의 마스터 키로 봉인됐는지다.
	KeyMatches bool      `json:"keyMatches"`
	PushedAt   string    `json:"pushedAt,omitempty"`
	AppVersion string    `json:"appVersion,omitempty"`
	CheckedAt  time.Time `json:"checkedAt"`
}

// Empty는 받아 올 것이 없는 원격인지다.
func (r *RemoteInfo) Empty() bool { return !r.Exists || (r.Rows == 0 && r.Owner == "") }

func (s *Service) d1Client(ctx context.Context) (*D1, *store.D1Settings, error) {
	cfg, err := s.st.D1Settings(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.Ready() {
		return nil, cfg, errors.New("계정 ID·데이터베이스 ID·API 토큰을 먼저 저장하세요")
	}
	return NewD1(cfg.AccountID, cfg.DatabaseID, cfg.APIToken), cfg, nil
}

// TestD1은 저장된 설정으로 D1에 붙어 원격 상태를 읽는다.
func (s *Service) TestD1(ctx context.Context) (*RemoteInfo, error) {
	c, _, err := s.d1Client(ctx)
	if err != nil {
		return nil, err
	}
	info, err := s.remoteInfo(ctx, c)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.d1.Remote = info
	s.mu.Unlock()
	return info, nil
}

func (s *Service) remoteInfo(ctx context.Context, c *D1) (*RemoteInfo, error) {
	me, err := s.st.InstanceID(ctx)
	if err != nil {
		return nil, err
	}
	local, _ := s.st.SchemaVersion(ctx)
	info := &RemoteInfo{LocalSchema: local, CheckedAt: time.Now()}
	res, err := c.Exec(ctx, []Stmt{
		{SQL: `SELECT k, v FROM dbstudio_meta`},
		{SQL: `SELECT count(*) AS n FROM dbstudio_rows`},
	})
	if isNoSuchTable(err) {
		info.KeyMatches = true
		return info, nil
	}
	if err != nil {
		return nil, err
	}
	info.Exists = true
	meta := map[string]string{}
	if len(res) > 0 {
		for _, r := range res[0].Results {
			meta[str(r["k"])] = str(r["v"])
		}
	}
	if len(res) > 1 && len(res[1].Results) > 0 {
		info.Rows = num(res[1].Results[0]["n"])
	}
	info.State = meta["state"]
	info.Owner = meta["owner"]
	info.OwnerName = meta["owner_name"]
	info.Mine = info.Owner != "" && info.Owner == me
	info.SchemaVersion, _ = strconv.Atoi(meta["schema_version"])
	info.PushedAt = meta["pushed_at"]
	info.AppVersion = meta["app_version"]
	fp := meta["key_fp"]
	info.KeyMatches = fp == "" || fp == s.st.Secret().Fingerprint()
	return info, nil
}

// batcher는 문장을 모아 한도에 닿으면 보낸다.
type batcher struct {
	ctx  context.Context
	c    *D1
	buf  []Stmt
	size int
	sent int
}

func (b *batcher) add(st Stmt) error {
	if len(b.buf) >= maxBatchStmts || (len(b.buf) > 0 && b.size+st.size() > maxBatchBytes) {
		if _, err := b.flush(nil); err != nil {
			return err
		}
	}
	b.buf = append(b.buf, st)
	b.size += st.size()
	return nil
}

// flush는 모은 것(과 extra)을 보낸다. 결과는 마지막 요청의 것이다.
func (b *batcher) flush(extra []Stmt) ([]D1Result, error) {
	stmts := append(b.buf, extra...)
	if len(stmts) == 0 {
		return nil, nil
	}
	res, err := b.c.Exec(b.ctx, stmts)
	if err != nil {
		return nil, err
	}
	b.sent += len(b.buf)
	b.buf, b.size = nil, 0
	return res, nil
}

// errNotOwner는 다른 환경이 이 D1의 주인이 되었다는 뜻이다.
var errNotOwner = errors.New("다른 환경이 이 D1을 가져가 주인이 되었습니다. 이 서버는 더 이상 올리지 않습니다 — " +
	"이 서버의 데이터로 덮어쓰거나, D1의 데이터를 다시 가져오세요")

// lastOwner는 batch 결과의 마지막 문장(ownerQuery)에서 주인을 읽는다.
func lastOwner(res []D1Result) string {
	if len(res) == 0 || len(res[len(res)-1].Results) == 0 {
		return ""
	}
	return str(res[len(res)-1].Results[0]["v"])
}

// ensureTriggers는 변경 추적을 켠다. 마스터에서는 클러스터가 모든 표에 이미 달아 두었다.
func (s *Service) ensureTriggers(ctx context.Context, force bool) error {
	if s.opt.IsMaster() {
		return nil
	}
	s.mu.Lock()
	on := s.triggersOn
	s.mu.Unlock()
	if on && !force {
		return nil
	}
	tables, err := s.st.CloudTables(ctx)
	if err != nil {
		return err
	}
	if _, err := s.st.InstallReplTriggersFor(ctx, tables); err != nil {
		return fmt.Errorf("변경 추적 트리거를 달지 못했습니다: %w", err)
	}
	s.mu.Lock()
	s.triggersOn = true
	s.mu.Unlock()
	return nil
}

// teardown은 동기화를 껐을 때 변경 추적을 걷어낸다. 다시 켜면 처음부터 올린다.
func (s *Service) teardown(ctx context.Context) {
	if !s.opt.IsMaster() {
		if err := s.st.DropReplTriggers(ctx); err != nil {
			s.log.Warn("변경 추적 트리거를 걷어내지 못했습니다", "err", err)
		}
		if err := s.st.ClearReplLog(ctx); err != nil {
			s.log.Warn("변경 로그를 비우지 못했습니다", "err", err)
		}
	}
	_ = s.st.SetLocalState(ctx, store.StateD1PushedSeq, "")
	_ = s.st.SetLocalState(ctx, store.StateD1FullNeeded, "")
	_ = s.st.SetLocalState(ctx, store.StateD1SkipList, "")
	s.mu.Lock()
	s.triggersOn = false
	s.mu.Unlock()
}

// startup은 서버가 뜰 때 한 번 부른다.
func (s *Service) startup(ctx context.Context) {
	cfg, err := s.st.D1Settings(ctx)
	if err != nil {
		s.log.Warn("D1 설정을 읽지 못했습니다", "err", err)
		return
	}
	_, synced, _ := s.st.LocalStateInt(ctx, store.StateD1PushedSeq)
	if !cfg.Enabled || !cfg.Ready() {
		// 예전에 켰다가 끈 채로 꺼진 서버라면 트리거가 남아 있다. 남은 트리거는 아무도
		// 비우지 않는 로그를 쌓는다.
		if !s.opt.IsMaster() {
			_ = s.st.DropReplTriggers(ctx)
			_ = s.st.ClearReplLog(ctx)
		}
		return
	}
	if !synced {
		return // 아직 "올릴까, 가져올까"를 고르지 않았다
	}
	// 바이너리가 바뀌어 컬럼이 늘었을 수 있다. 트리거를 새로 단다(InstallReplTriggers 주석 참고).
	if err := s.ensureTriggers(ctx, true); err != nil {
		s.log.Error("D1 동기화를 시작하지 못했습니다", "err", err)
		s.setD1Error(err)
		return
	}
	// 스키마가 바뀌었으면 전체를 다시 올린다. 표를 새로 만들어 옮기는 마이그레이션은 rowid를
	// 바꿀 수 있는데, 그러면 행 단위 변경이 원격의 엉뚱한 줄을 가리킨다.
	local, _ := s.st.SchemaVersion(ctx)
	if prev, ok, _ := s.st.LocalStateInt(ctx, store.StateD1SchemaVer); !ok || int(prev) != local {
		_ = s.st.SetLocalState(ctx, store.StateD1FullNeeded, "schema")
	}
}

// tickD1은 주기가 되었으면 한 번 동기화한다.
func (s *Service) tickD1(ctx context.Context) {
	cfg, err := s.st.D1Settings(ctx)
	if err != nil {
		s.setD1Error(err)
		return
	}
	if !cfg.Enabled || !cfg.Ready() {
		s.mu.Lock()
		wasOn := s.triggersOn
		s.d1.State, s.d1.Message, s.d1.NextSyncAt = "off", "", nil
		s.mu.Unlock()
		if wasOn {
			s.teardown(ctx)
		}
		return
	}
	s.mu.Lock()
	due := time.Since(s.lastD1At) >= cfg.Interval()
	s.mu.Unlock()
	if !due {
		return
	}
	if !s.opMu.TryLock() {
		return // 가져오기·백업이 도는 중이다. 다음 틱에 한다
	}
	defer s.opMu.Unlock()

	s.mu.Lock()
	s.lastD1At = time.Now()
	next := s.lastD1At.Add(cfg.Interval())
	s.d1.NextSyncAt = &next
	s.mu.Unlock()

	sent, err := s.syncOnce(ctx)
	if err != nil {
		s.setD1Error(err)
		return
	}
	now := time.Now()
	s.mu.Lock()
	if s.d1.State != "setup" {
		s.d1.State, s.d1.Message = "ok", ""
	}
	s.d1.LastSyncAt = &now
	s.d1.LastSent = sent
	s.d1.LastError, s.d1.ErrorAt = "", nil
	s.mu.Unlock()
}

func (s *Service) setD1Error(err error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(err, errNotOwner) {
		s.d1.State = "conflict"
	} else {
		s.d1.State = "error"
	}
	s.d1.LastError = firstLine(err)
	s.d1.ErrorAt = &now
}

// syncOnce는 지금까지의 변경을 올린다. 처음이면 전체를 올린다. 올린 행 수를 돌려준다.
func (s *Service) syncOnce(ctx context.Context) (int, error) {
	c, _, err := s.d1Client(ctx)
	if err != nil {
		return 0, err
	}
	_, synced, err := s.st.LocalStateInt(ctx, store.StateD1PushedSeq)
	if err != nil {
		return 0, err
	}
	full, _ := s.st.LocalState(ctx, store.StateD1FullNeeded)

	if !synced || full != "" {
		info, err := s.remoteInfo(ctx, c)
		if err != nil {
			return 0, err
		}
		s.mu.Lock()
		s.d1.Remote = info
		s.mu.Unlock()
		// 스스로 전체를 올려도 되는 경우는 둘뿐이다: 원격이 비어 있거나, 원격의 주인이 나다.
		// 남의 데이터가 있는 D1을 말없이 덮어쓰는 것은 동기화가 아니라 파괴다.
		if !info.Empty() && !info.Mine {
			s.mu.Lock()
			s.d1.State = "setup"
			s.d1.Message = "D1 에 다른 환경의 데이터가 있습니다. 가져올지, 이 서버의 데이터로 덮어쓸지 고르세요"
			s.mu.Unlock()
			if synced {
				return 0, errNotOwner
			}
			return 0, nil
		}
		return s.fullPush(ctx, c, info.Empty(), nil)
	}
	return s.pushChanges(ctx, c)
}

// pushChanges는 마지막으로 보낸 뒤의 변경을 모아 보낸다.
func (s *Service) pushChanges(ctx context.Context, c *D1) (int, error) {
	pushed, _, err := s.st.LocalStateInt(ctx, store.StateD1PushedSeq)
	if err != nil {
		return 0, err
	}
	lo, hi, err := s.st.ReplBounds(ctx)
	if err != nil {
		return 0, err
	}
	// 보내지 않는 표가 늘었으면(이 바이너리가 새 판) 예전 판이 올려 둔 그 표의 행을 지운다.
	// 그대로 두면 원격에 영영 갱신되지 않는 옛 행이 남아 자리만 차지한다.
	skip := store.CloudSkipList()
	sentSkip, _ := s.st.LocalState(ctx, store.StateD1SkipList)
	purge := sentSkip != strings.Join(skip, ",")
	if hi <= pushed && !purge {
		return 0, nil
	}
	// 로그가 잘려 보낸 지점과 이어지지 않으면(클러스터 마스터의 정리, 오래 끊겼던 경우)
	// 변경만으로는 따라잡을 수 없다. 전체를 다시 올린다 — 조용히 건너뛰면 그 사이의 변경이
	// 영원히 빠진 채로 갈라진다.
	if (lo > pushed+1 && pushed > 0) || hi-pushed > maxPendingLog {
		if hi-pushed > maxPendingLog && !s.opt.IsMaster() {
			_ = s.st.ClearReplLog(ctx)
		}
		return s.fullPush(ctx, c, false, nil)
	}

	me, err := s.st.InstanceID(ctx)
	if err != nil {
		return 0, err
	}
	type key struct {
		tbl string
		rid int64
	}
	latest := map[key]store.ReplChange{}
	last := pushed
	for last < hi {
		chs, err := s.st.ReplChanges(ctx, last, 5000)
		if err != nil {
			return 0, err
		}
		if len(chs) == 0 {
			break
		}
		for _, ch := range chs {
			if ch.Seq > hi {
				break
			}
			last = ch.Seq
			if store.CloudSkipped(ch.Table) {
				continue
			}
			latest[key{ch.Table, ch.RowID}] = ch
		}
	}

	changes := make([]store.ReplChange, 0, len(latest))
	for _, ch := range latest {
		changes = append(changes, ch)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Seq < changes[j].Seq })

	if len(changes) > 0 || purge {
		b := &batcher{ctx: ctx, c: c}
		if purge {
			if err := b.add(deleteTables(skip, me)); err != nil {
				return 0, err
			}
		}
		for _, ch := range changes {
			var st Stmt
			if ch.Op == "delete" {
				st = deleteRow(ch.Table, ch.RowID, me)
			} else {
				st = upsertRow(ch.Table, ch.RowID, string(ch.Row), ch.At, me)
			}
			if err := b.add(st); err != nil {
				return 0, err
			}
		}
		res, err := b.flush([]Stmt{
			setMeta("pushed_seq", strconv.FormatInt(last, 10), me),
			setMeta("pushed_at", time.Now().UTC().Format(time.RFC3339), me),
			ownerQuery,
		})
		if err != nil {
			return 0, err
		}
		if owner := lastOwner(res); owner != me {
			_ = s.st.SetLocalState(ctx, store.StateD1FullNeeded, "owner")
			return 0, errNotOwner
		}
	}

	if err := s.st.SetLocalState(ctx, store.StateD1PushedSeq, strconv.FormatInt(last, 10)); err != nil {
		return 0, err
	}
	if purge {
		_ = s.st.SetLocalState(ctx, store.StateD1SkipList, strings.Join(skip, ","))
	}
	if !s.opt.IsMaster() {
		if _, err := s.st.PruneReplLogUpTo(ctx, last); err != nil {
			s.log.Warn("보낸 변경 로그를 지우지 못했습니다", "err", err)
		}
	}
	return len(changes), nil
}

// fullPush는 메타 DB 전체를 D1에 올린다. claim이면 원격의 주인을 이 서버로 바꾼다.
func (s *Service) fullPush(ctx context.Context, c *D1, claim bool, j *Job) (int, error) {
	me, err := s.st.InstanceID(ctx)
	if err != nil {
		return 0, err
	}
	// 변경 추적을 스냅샷보다 **먼저** 켠다. 그래야 스냅샷 직후의 변경이 로그에 남아
	// 다음 동기화로 이어진다.
	if err := s.ensureTriggers(ctx, false); err != nil {
		return 0, err
	}
	tables, err := s.st.CloudTables(ctx)
	if err != nil {
		return 0, err
	}
	s.stage(j, "원격 표 준비", 0)
	if _, err := c.Exec(ctx, remoteSchema); err != nil {
		return 0, err
	}
	if !claim {
		res, err := c.Exec(ctx, []Stmt{ownerQuery})
		if err != nil {
			return 0, err
		}
		if owner := lastOwner(res); owner != "" && owner != me {
			return 0, errNotOwner
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := c.Exec(ctx, []Stmt{
		claimMeta("owner", me),
		claimMeta("owner_name", s.opt.NodeName),
		claimMeta("owner_at", now),
		claimMeta("state", "pushing"),
		{SQL: `DELETE FROM dbstudio_rows`},
	}); err != nil {
		return 0, err
	}

	s.stage(j, "메타 DB 스냅샷", 0)
	snap, err := s.tempPath("push.db")
	if err != nil {
		return 0, err
	}
	defer os.Remove(snap)
	if err := s.st.SnapshotTo(ctx, snap); err != nil {
		return 0, err
	}

	s.stage(j, "행 올리기", 0)
	b := &batcher{ctx: ctx, c: c}
	count := 0
	seq, err := store.ExportSnapshotRows(ctx, snap, tables, func(tbl string, rid int64, row []byte) error {
		count++
		if count%500 == 0 {
			s.stage(j, "행 올리기", int64(count))
		}
		return b.add(upsertRow(tbl, rid, string(row), now, me))
	})
	if err != nil {
		return 0, err
	}
	local, _ := s.st.SchemaVersion(ctx)
	res, err := b.flush([]Stmt{
		setMeta("format", remoteFormat, me),
		setMeta("state", "ready", me),
		setMeta("pushed_seq", strconv.FormatInt(seq, 10), me),
		setMeta("pushed_at", time.Now().UTC().Format(time.RFC3339), me),
		setMeta("schema_version", strconv.Itoa(local), me),
		setMeta("key_fp", s.st.Secret().Fingerprint(), me),
		setMeta("app_version", appVersion(), me),
		ownerQuery,
	})
	if err != nil {
		return 0, err
	}
	if lastOwner(res) != me {
		return 0, errNotOwner
	}

	if err := s.markSynced(ctx, seq, local); err != nil {
		return 0, err
	}
	// 원격을 비우고 보낼 표만 올렸으니 치울 것이 없다.
	_ = s.st.SetLocalState(ctx, store.StateD1SkipList, strings.Join(store.CloudSkipList(), ","))
	s.stage(j, "완료", int64(count))
	return count, nil
}

// markSynced는 "seq까지 원격과 같다"를 기록한다.
func (s *Service) markSynced(ctx context.Context, seq int64, schema int) error {
	if err := s.st.SetLocalState(ctx, store.StateD1PushedSeq, strconv.FormatInt(seq, 10)); err != nil {
		return err
	}
	_ = s.st.SetLocalState(ctx, store.StateD1FullNeeded, "")
	_ = s.st.SetLocalState(ctx, store.StateD1SchemaVer, strconv.Itoa(schema))
	if !s.opt.IsMaster() {
		if _, err := s.st.PruneReplLogUpTo(ctx, seq); err != nil {
			s.log.Warn("보낸 변경 로그를 지우지 못했습니다", "err", err)
		}
	}
	s.mu.Lock()
	s.d1.State, s.d1.Message = "ok", ""
	s.d1.LastError, s.d1.ErrorAt = "", nil
	s.mu.Unlock()
	return nil
}

func (s *Service) stage(j *Job, stage string, done int64) {
	if j != nil {
		s.progress(j, stage, done)
	}
}

// StartPush는 이 서버의 데이터로 D1을 덮어쓰고 주인이 된다.
func (s *Service) StartPush() (*Job, error) {
	return s.startJob("d1.push", "D1 에 이 서버의 데이터 올리기", func(ctx context.Context, j *Job) (string, error) {
		c, _, err := s.d1Client(ctx)
		if err != nil {
			return "", err
		}
		n, err := s.fullPush(ctx, c, true, j)
		if err != nil {
			s.setD1Error(err)
			return "", err
		}
		s.mu.Lock()
		s.d1.Remote = nil
		s.lastD1At = time.Now()
		s.mu.Unlock()
		return fmt.Sprintf("%d개 행을 올렸습니다. 이제 이 서버가 D1의 주인입니다", n), nil
	})
}

// StartPull은 D1의 데이터를 받아 이 서버의 데이터를 덮어쓰고 주인이 된다.
//
// after는 덮어쓴 **뒤에** 불린다. 감사 기록을 여기서 남겨야 한다 — 시작할 때 남긴 기록은
// 감사 로그 표째로 받아 온 것에 덮여 사라진다.
func (s *Service) StartPull(after func(context.Context)) (*Job, error) {
	return s.startJob("d1.pull", "D1 의 데이터 가져오기", func(ctx context.Context, j *Job) (string, error) {
		n, err := s.pull(ctx, j)
		if err != nil {
			return "", err
		}
		if after != nil {
			after(ctx)
		}
		j.Relogin = true
		return fmt.Sprintf("%d개 행을 가져왔습니다. 다시 로그인하세요", n), nil
	})
}

func (s *Service) pull(ctx context.Context, j *Job) (int, error) {
	c, _, err := s.d1Client(ctx)
	if err != nil {
		return 0, err
	}
	info, err := s.remoteInfo(ctx, c)
	if err != nil {
		return 0, err
	}
	switch {
	case info.Empty():
		return 0, errors.New("D1 이 비어 있어 가져올 것이 없습니다. 이 서버의 데이터를 올리세요")
	case info.State != "ready":
		return 0, errors.New("D1 의 데이터가 올리는 도중에 멈춘 상태입니다. 원래 서버에서 다시 올린 뒤 가져오세요")
	case !info.KeyMatches:
		return 0, errors.New("D1 의 데이터는 다른 마스터 키로 봉인되어 있습니다. 원래 서버와 같은 키" +
			"(DBSTUDIO_MASTER_KEY 또는 master.key)로 이 서버를 띄운 뒤 가져오세요 — 그러지 않으면 " +
			"저장된 DB 자격증명을 하나도 풀 수 없습니다")
	case info.SchemaVersion > info.LocalSchema:
		return 0, fmt.Errorf("D1 의 데이터는 더 새 버전의 DB Studio 가 썼습니다(스키마 %d > %d). 이 서버를 먼저 올리세요",
			info.SchemaVersion, info.LocalSchema)
	}

	// 먼저 전부 내려받는다. 받는 도중에 망이 끊기면 아무것도 바꾸지 않은 채로 끝나야 한다.
	spool, err := s.tempPath("pull.jsonl")
	if err != nil {
		return 0, err
	}
	defer os.Remove(spool)
	got, err := s.download(ctx, c, spool, j)
	if err != nil {
		return 0, err
	}

	s.stage(j, "덮어쓰기 전 안전 사본", got)
	backup, err := s.safetyCopy(ctx, "pull")
	if err != nil {
		return 0, fmt.Errorf("덮어쓰기 전 안전 사본을 만들지 못해 멈췄습니다: %w", err)
	}
	restoreSettings, err := s.keepCloudSettings(ctx)
	if err != nil {
		return 0, err
	}

	s.stage(j, "이 서버에 적용", got)
	n, err := s.st.ReplaceCloudTables(ctx, func(put func(string, int64, []byte) error) error {
		f, err := os.Open(spool)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			var line spoolLine
			if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
				return err
			}
			if err := put(line.T, line.R, []byte(line.W)); err != nil {
				return err
			}
		}
		return sc.Err()
	})
	if err != nil {
		return 0, fmt.Errorf("가져온 데이터를 적용하지 못했습니다(이 서버는 바뀌지 않았습니다): %w", err)
	}
	if err := restoreSettings(ctx); err != nil {
		s.log.Warn("클라우드 설정을 되살리지 못했습니다", "err", err)
	}
	s.log.Info("D1 에서 메타 DB 를 가져왔습니다", "rows", n, "safetyCopy", backup)

	// 받아 온 순간 이 서버가 주인이다. 옛 주인은 다음 동기화에서 그 사실을 알고 멈춘다.
	me, _ := s.st.InstanceID(ctx)
	if _, err := c.Exec(ctx, []Stmt{
		claimMeta("owner", me),
		claimMeta("owner_name", s.opt.NodeName),
		claimMeta("owner_at", time.Now().UTC().Format(time.RFC3339)),
	}); err != nil {
		return 0, fmt.Errorf("데이터는 가져왔지만 D1 의 주인을 바꾸지 못했습니다: %w", err)
	}
	if err := s.ensureTriggers(ctx, true); err != nil {
		return 0, err
	}
	// 지금 이 서버와 원격은 같다. 적용하면서 트리거가 남긴 로그는 보낼 필요가 없다.
	_, hi, _ := s.st.ReplBounds(ctx)
	local, _ := s.st.SchemaVersion(ctx)
	if err := s.markSynced(ctx, hi, local); err != nil {
		return 0, err
	}
	// 받아 온 D1은 옛 판이 올린 것일 수 있다 — 지금은 보내지 않는 표의 행이 남아 있으면
	// 다음 동기화에서 치운다.
	_ = s.st.SetLocalState(ctx, store.StateD1SkipList, "")
	// 원격이 옛 스키마로 쓴 것이면 새 컬럼을 채워 다시 올려 둔다.
	if info.SchemaVersion < local {
		_ = s.st.SetLocalState(ctx, store.StateD1FullNeeded, "schema")
	}
	s.mu.Lock()
	s.d1.Remote = nil
	s.lastD1At = time.Now()
	s.mu.Unlock()
	s.afterReplace("D1 에서 데이터를 가져와 이 서버의 데이터가 바뀌었습니다. 다시 여세요")
	return n, nil
}

// spoolLine은 내려받은 행 하나다(가져오기의 임시 파일 한 줄).
type spoolLine struct {
	T string `json:"t"`
	R int64  `json:"r"`
	W string `json:"w"`
}

func (s *Service) download(ctx context.Context, c *D1, path string, j *Job) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)

	var got int64
	lastTbl, lastRid := "", int64(-1<<62)
	for {
		res, err := c.Exec(ctx, []Stmt{{
			// 열쇠 순서로 이어 읽는다. OFFSET은 뒤로 갈수록 앞의 행을 다시 세므로 느려진다.
			SQL: `SELECT tbl, rid, row FROM dbstudio_rows WHERE tbl > ? OR (tbl = ? AND rid > ?)
				ORDER BY tbl, rid LIMIT ` + strconv.Itoa(pullPage),
			Params: []string{lastTbl, lastTbl, strconv.FormatInt(lastRid, 10)},
		}})
		if err != nil {
			return 0, err
		}
		if len(res) == 0 || len(res[0].Results) == 0 {
			break
		}
		for _, r := range res[0].Results {
			line := spoolLine{T: str(r["tbl"]), R: num(r["rid"]), W: str(r["row"])}
			if err := enc.Encode(line); err != nil {
				return 0, err
			}
			lastTbl, lastRid = line.T, line.R
			got++
		}
		s.stage(j, "내려받기", got)
		if len(res[0].Results) < pullPage {
			break
		}
	}
	if err := w.Flush(); err != nil {
		return 0, err
	}
	return got, nil
}

// ResetD1은 "어디까지 보냈는가"를 잊는다. 연결할 D1을 바꿨을 때 부른다 — 옛 D1에 보낸
// 지점을 새 D1에 이어 쓰면 새 D1에는 그 앞의 데이터가 영영 없다.
func (s *Service) ResetD1(ctx context.Context) {
	_ = s.st.SetLocalState(ctx, store.StateD1PushedSeq, "")
	_ = s.st.SetLocalState(ctx, store.StateD1FullNeeded, "")
	_ = s.st.SetLocalState(ctx, store.StateD1SkipList, "")
	s.mu.Lock()
	s.d1 = D1Status{State: "off"}
	s.mu.Unlock()
	s.Kick()
}
