package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 클라우드 연결(Cloudflare D1 동기화, R2·S3 백업)이 메타 DB에 기대는 것들.
//
// 저장소는 클라우드를 모른다. 여기 있는 것은 "어떤 표를 보낼 것인가", "행을 어떤 모양으로
// 꺼내고 넣을 것인가", "이 노드가 어디까지 보냈는가"뿐이고, HTTP는 cloud 패키지가 한다.

// cloudSkip은 D1로 보내지 않는 표다. 복제하지 않는 표(replSkip)는 당연히 빠진다.
//
// 기준은 "새 환경에서 받았을 때 쓸모가 있는가"와 "쓰기 빈도"다. 여기 있는 표는 이 노드가
// 돌면서 다시 만들어지거나(지표·상태), 다른 컴퓨터로 옮기면 안 되거나(세션·로그인 중간
// 상태), 이 배치에만 뜻이 있다(클러스터 노드 목록). 게다가 대부분 30초마다 덮어쓰이므로,
// 보내기 시작하면 D1의 쓰기 한도가 지표에 다 쓰인다.
var cloudSkip = map[string]bool{
	"metric_samples":   true, // 원본 지표. 보존 기간 48시간짜리 시계열이다
	"metric_hourly":    true, // 원본을 시간 단위로 접은 것. 이 노드가 수집한 만큼만 있다
	"events":           true, // 감시가 낸 경보. 열려 있는 동안 폴링마다 횟수가 오르고, 이 컴퓨터의 경보도 섞여 있다
	"host_samples":     true, // 이 컴퓨터의 지표
	"connection_state": true, // 폴링마다 덮어쓰는 "지금 상태"
	"host_state":       true,
	"sessions":         true, // 로그인은 환경마다 새로 한다. 세션을 옮기면 쿠키 하나로 두 곳에 들어간다
	"totp_challenges":  true, // 5분짜리 로그인 중간 상태
	"cluster_nodes":    true, // 이 배치의 노드 목록. 하트비트마다 바뀐다
}

// CloudSkipped는 D1로 보내지 않는 표인지 본다.
func CloudSkipped(table string) bool { return replSkip[table] || cloudSkip[table] }

// CloudSkipList는 cloudSkip의 표 이름을 정렬해 돌려준다. 목록이 늘었는지 알아보는 데 쓴다 —
// 늘어난 표의 행은 예전 판이 이미 D1에 올려 두었으므로 지워야 한다.
func CloudSkipList() []string {
	out := make([]string, 0, len(cloudSkip))
	for t := range cloudSkip {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// CloudTables는 D1로 보낼 표 목록이다.
func (s *Store) CloudTables(ctx context.Context) ([]string, error) {
	all, err := s.ReplTables(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, t := range all {
		if !cloudSkip[t] {
			out = append(out, t)
		}
	}
	return out, nil
}

// ---------- 이 노드의 상태 (repl_state) ----------
//
// repl_state는 복제되지 않는 유일한 표다. "이 노드가 D1에 어디까지 보냈는가", "마지막
// 정기 백업이 언제였는가"는 노드마다 달라야 하고, 원격에서 받아 온 데이터에 덮여서도
// 안 된다(받아 온 순간 "다 보냈다"로 읽히면 그 뒤의 변경이 영원히 안 간다).

// 로컬 상태 키.
const (
	StateInstanceID     = "cloud.instance_id"
	StateD1PushedSeq    = "d1.pushed_seq"
	StateD1FullNeeded   = "d1.full_needed"
	StateD1SchemaVer    = "d1.schema_version"
	StateD1SkipList     = "d1.skip_list"
	StateObjLastBackup  = "objstore.last_backup_at"
	StateObjLastAttempt = "objstore.last_attempt_at"
)

// LocalState는 이 노드의 상태 값 하나를 읽는다. 없으면 빈 문자열이다.
func (s *Store) LocalState(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM repl_state WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetLocalState는 이 노드의 상태 값 하나를 쓴다. 빈 값이면 지운다.
func (s *Store) SetLocalState(ctx context.Context, key, value string) error {
	if value == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM repl_state WHERE k = ?`, key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repl_state (k, v) VALUES (?, ?)
		ON CONFLICT (k) DO UPDATE SET v = excluded.v`, key, value)
	return err
}

// InstanceID는 이 설치본의 식별자다. 처음 부르면 만든다.
//
// D1의 "주인"을 가리는 데 쓴다. 두 환경이 같은 D1에 번갈아 쓰면 서로의 변경을 덮어쓰고,
// 그 사실은 어느 쪽에서도 오류로 보이지 않는다. 주인이 하나라고 정해 두면 주인이 아닌
// 쪽은 쓰기를 멈추고 그 이유를 화면에 말할 수 있다.
func (s *Store) InstanceID(ctx context.Context) (string, error) {
	id, err := s.LocalState(ctx, StateInstanceID)
	if err != nil || id != "" {
		return id, err
	}
	id = uuid.NewString()
	return id, s.SetLocalState(ctx, StateInstanceID, id)
}

// LocalStateInt는 정수 상태 값을 읽는다. 없거나 깨졌으면 0이다.
func (s *Store) LocalStateInt(ctx context.Context, key string) (int64, bool, error) {
	v, err := s.LocalState(ctx, key)
	if err != nil || v == "" {
		return 0, false, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, nil
	}
	return n, true, nil
}

// LocalStateTime은 시각 상태 값을 읽는다. 없으면 영값이다.
func (s *Store) LocalStateTime(ctx context.Context, key string) (time.Time, error) {
	v, err := s.LocalState(ctx, key)
	if err != nil {
		return time.Time{}, err
	}
	return parseTime(v), nil
}

// SetLocalStateTime은 시각 상태 값을 쓴다.
func (s *Store) SetLocalStateTime(ctx context.Context, key string, t time.Time) error {
	return s.SetLocalState(ctx, key, formatTime(t))
}

// PruneReplLogUpTo는 seq 이하의 복제 로그를 지운다.
//
// 단일 서버에서 D1 동기화가 쓴다. 클러스터 마스터에서는 부르지 않는다 — 리플리카가 아직
// 받지 않은 줄일 수 있고, 그쪽 정리는 클러스터가 기간·줄 수로 한다.
func (s *Store) PruneReplLogUpTo(ctx context.Context, seq int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM repl_log WHERE seq <= ?`, seq)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClearReplLog는 복제 로그를 비운다(동기화를 끌 때).
func (s *Store) ClearReplLog(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM repl_log`)
	return err
}

// ---------- 행 꺼내기·넣기 ----------
//
// 행은 복제 로그와 같은 모양(JSON 객체, BLOB은 16진 문자열)으로 오간다. 같은 모양이어야
// 트리거가 쓴 변경과 스냅샷에서 꺼낸 행을 받는 쪽이 구분하지 않고 넣을 수 있다.

// ExportSnapshotRows는 스냅샷 파일(SnapshotTo로 뜬 것)에서 tables의 행을 모두 꺼낸다.
//
// 돌려주는 seq는 그 스냅샷이 담고 있는 복제 로그의 끝이다. 이 값 이후의 변경만 이어
// 보내면 스냅샷과 합쳐 빠짐없이 같아진다 — 실행 중인 DB에서 직접 읽으면 읽는 사이에
// 들어온 변경이 반쯤 섞여 "어디부터 이어 보낼지"를 정할 수 없다.
func ExportSnapshotRows(ctx context.Context, path string, tables []string,
	emit func(table string, rid int64, row []byte) error) (int64, error) {
	db, err := sql.Open("sqlite", strings.ReplaceAll(path, "\\", "/")+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var seq sql.NullInt64
	// 복제 로그 표가 없는 스냅샷은 없다(0029 이후). 그래도 없다면 0에서 시작한다.
	_ = db.QueryRowContext(ctx, `SELECT max(seq) FROM repl_log`).Scan(&seq)

	for _, t := range tables {
		var exists int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?`, t).Scan(&exists); err != nil {
			return 0, err
		}
		if exists == 0 {
			continue
		}
		if err := exportTable(ctx, db, t, emit); err != nil {
			return 0, fmt.Errorf("%s: %w", t, err)
		}
	}
	return seq.Int64, nil
}

func exportTable(ctx context.Context, db *sql.DB, table string,
	emit func(table string, rid int64, row []byte) error) error {
	cols, blobs, err := tableColumns(ctx, db, table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = fmt.Sprintf("%q", c)
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT rowid, %s FROM %q ORDER BY rowid`,
		strings.Join(quoted, ", "), table))
	if err != nil {
		return err
	}
	defer rows.Close()

	vals := make([]any, len(cols)+1)
	ptrs := make([]any, len(vals))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		rid, _ := vals[0].(int64)
		obj := make(map[string]any, len(cols))
		for i, c := range cols {
			v := vals[i+1]
			switch x := v.(type) {
			case []byte:
				if blobs[c] {
					// 트리거의 hex()와 같은 대문자 16진이다.
					v = strings.ToUpper(hex.EncodeToString(x))
				} else {
					v = string(x)
				}
			}
			obj[c] = v
		}
		raw, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		if err := emit(table, rid, raw); err != nil {
			return err
		}
	}
	return rows.Err()
}

// tableColumns는 columnsOf와 같되 아무 핸들에서나 읽는다(스냅샷 파일용).
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, map[string]bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var cols []string
	blobs := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, nil, err
		}
		cols = append(cols, name)
		if strings.Contains(strings.ToUpper(typ), "BLOB") {
			blobs[name] = true
		}
	}
	return cols, blobs, rows.Err()
}

// RowSource는 넣을 행을 하나씩 내놓는다. put을 부를 때마다 한 행이 들어간다.
type RowSource func(put func(table string, rid int64, row []byte) error) error

// ReplaceCloudTables는 D1로 보내는 표들을 rows의 내용으로 통째로 바꾼다. 한 트랜잭션이다.
//
// D1로 보내지 않는 표(지표·세션)는 그대로 둔다. 그중 사라진 부모를 가리키게 된 행은
// 지운다(replaceOnConn 참고) — 받아 온 사용자 목록에 없는 사람의 세션이 대표적이다.
func (s *Store) ReplaceCloudTables(ctx context.Context, rows RowSource) (int, error) {
	tables, err := s.CloudTables(ctx)
	if err != nil {
		return 0, err
	}
	want := map[string]bool{}
	for _, t := range tables {
		want[t] = true
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	count := 0
	err = s.replaceOnConn(ctx, conn, tables, func(tx *sql.Tx) error {
		ins := newRowInserter(s, tx)
		return rows(func(table string, rid int64, row []byte) error {
			// 이 바이너리에 없는 표(더 새 버전이 보낸 것)나 보내지 않는 표는 건너뛴다.
			if !want[table] {
				return nil
			}
			count++
			return ins.insert(ctx, table, rid, row)
		})
	})
	return count, err
}

// rowInserter는 JSON 행을 표에 넣는다. 표마다 컬럼 정보를 한 번만 읽는다.
type rowInserter struct {
	s     *Store
	tx    *sql.Tx
	known map[string]map[string]bool
	blobs map[string]map[string]bool
}

func newRowInserter(s *Store, tx *sql.Tx) *rowInserter {
	return &rowInserter{s: s, tx: tx,
		known: map[string]map[string]bool{}, blobs: map[string]map[string]bool{}}
}

func (r *rowInserter) insert(ctx context.Context, table string, rid int64, raw []byte) error {
	known, ok := r.known[table]
	if !ok {
		cols, blobs, err := r.s.columnsOf(ctx, table)
		if err != nil {
			return err
		}
		known = map[string]bool{}
		for _, c := range cols {
			known[c] = true
		}
		r.known[table], r.blobs[table] = known, blobs
	}
	blobs := r.blobs[table]

	// 숫자를 json.Number로 받는다. float64로 받으면 2^53을 넘는 정수(나노초 시각,
	// 큰 카운터)의 끝자리가 조용히 바뀐다.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var row map[string]any
	if err := dec.Decode(&row); err != nil {
		return fmt.Errorf("%s 행 %d 를 읽지 못했습니다: %w", table, rid, err)
	}

	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	// 키 순서를 고정한다. 순서가 흔들리면 같은 표의 문장이 행마다 달라진다.
	sort.Strings(keys)

	names := []string{`"rowid"`}
	holes := []string{"?"}
	args := []any{rid}
	for _, k := range keys {
		v := row[k]
		// 보낸 쪽에 있고 여기에 없는 컬럼은 버전 차이다. 넣으려 하면 표 전체가 실패한다.
		if !known[k] {
			continue
		}
		switch x := v.(type) {
		case json.Number:
			if n, err := x.Int64(); err == nil {
				v = n
			} else if f, err := x.Float64(); err == nil {
				v = f
			} else {
				v = x.String()
			}
		case string:
			if blobs[k] {
				b, err := hex.DecodeString(x)
				if err != nil {
					return fmt.Errorf("%s.%s: 16진 값을 되돌리지 못했습니다: %w", table, k, err)
				}
				v = b
			}
		case bool:
			if x {
				v = 1
			} else {
				v = 0
			}
		case map[string]any, []any:
			// 트리거는 컬럼 값을 문자열로 싣는다. 객체가 왔다면 JSON 컬럼을 그대로
			// 실은 것이므로 문자열로 되돌린다.
			b, _ := json.Marshal(x)
			v = string(b)
		}
		names = append(names, fmt.Sprintf("%q", k))
		holes = append(holes, "?")
		args = append(args, v)
	}
	stmt := fmt.Sprintf(`INSERT INTO %q (%s) VALUES (%s)`,
		table, strings.Join(names, ", "), strings.Join(holes, ", "))
	if _, err := r.tx.ExecContext(ctx, stmt, args...); err != nil {
		return fmt.Errorf("%s 행 %d 넣기 실패: %w", table, rid, err)
	}
	return nil
}

// InvalidateCaches는 메타 DB를 통째로 바꾼 뒤 메모리에 남은 값을 버린다.
func (s *Store) InvalidateCaches() { s.invalidatePolicy() }

// ---------- 설정 ----------

// 클라우드 설정 키(app_settings).
const (
	SettingCloudD1     = "cloud.d1"
	SettingCloudObject = "cloud.objstore"
)

// D1Settings는 Cloudflare D1 동기화 설정이다.
type D1Settings struct {
	Enabled    bool   `json:"enabled"`
	AccountID  string `json:"accountId"`
	DatabaseID string `json:"databaseId"`
	// APIToken은 D1 편집 권한이 있는 Cloudflare API 토큰이다. 저장할 때 봉인한다.
	APIToken string `json:"apiToken,omitempty"`
	// IntervalSec은 변경을 모아 보내는 주기(초)다.
	IntervalSec int `json:"intervalSec"`

	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy string     `json:"updatedBy,omitempty"`
}

// D1 동기화 주기의 범위. 너무 짧으면 Cloudflare API 한도(5분에 1,200번)를 다른 일과
// 나눠 쓰기 어렵고, 너무 길면 서버가 갑자기 죽었을 때 잃는 양이 커진다.
const (
	D1IntervalDefault = 30
	D1IntervalMin     = 10
	D1IntervalMax     = 3600
)

// Ready는 접속에 필요한 값이 다 있는지다.
func (d D1Settings) Ready() bool {
	return strings.TrimSpace(d.AccountID) != "" && strings.TrimSpace(d.DatabaseID) != "" &&
		strings.TrimSpace(d.APIToken) != ""
}

// Interval은 실제로 쓸 주기다.
func (d D1Settings) Interval() time.Duration {
	n := d.IntervalSec
	if n <= 0 {
		n = D1IntervalDefault
	}
	if n < D1IntervalMin {
		n = D1IntervalMin
	}
	if n > D1IntervalMax {
		n = D1IntervalMax
	}
	return time.Duration(n) * time.Second
}

// Masked는 화면에 돌려줄 사본이다.
func (d D1Settings) Masked() D1Settings {
	out := d
	out.APIToken = maskToken(d.APIToken)
	return out
}

// ObjectSettings는 백업을 올릴 오브젝트 스토리지(R2·S3·S3 호환) 설정이다.
type ObjectSettings struct {
	// Provider는 r2 | s3 | custom 이다. 엔드포인트를 정하는 방법만 다르다.
	Provider string `json:"provider"`
	// AccountID는 R2의 계정 ID다(엔드포인트가 <account>.r2.cloudflarestorage.com).
	AccountID string `json:"accountId,omitempty"`
	// Endpoint는 custom에서 쓰는 주소다(https://minio.example.com:9000). s3에서 비우면 리전으로 정한다.
	Endpoint string `json:"endpoint,omitempty"`
	Region   string `json:"region,omitempty"`
	Bucket   string `json:"bucket"`
	// Prefix는 백업 객체 이름 앞에 붙는 경로다(dbstudio/). 한 버킷을 여러 설치본이 나눠 쓸 수 있다.
	Prefix      string `json:"prefix,omitempty"`
	AccessKey   string `json:"accessKey"`
	SecretKey   string `json:"secretKey,omitempty"`
	VirtualHost bool   `json:"virtualHost,omitempty"`
	// Encrypt면 마스터 키에서 파생한 키로 백업 파일을 암호화해 올린다.
	Encrypt bool `json:"encrypt"`

	// 정기 백업. Every 시간마다, At(HH:MM, 서버 시각)을 기준점으로 돈다.
	ScheduleEnabled bool   `json:"scheduleEnabled"`
	EveryHours      int    `json:"everyHours"`
	At              string `json:"at"`
	// Keep은 남길 백업 수다. 넘으면 오래된 것부터 지운다. 0이면 지우지 않는다.
	Keep int `json:"keep"`

	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy string     `json:"updatedBy,omitempty"`
}

// Ready는 접속에 필요한 값이 다 있는지다.
func (o ObjectSettings) Ready() bool {
	if strings.TrimSpace(o.Bucket) == "" || strings.TrimSpace(o.AccessKey) == "" ||
		strings.TrimSpace(o.SecretKey) == "" {
		return false
	}
	switch o.Provider {
	case "r2":
		return strings.TrimSpace(o.AccountID) != ""
	case "s3":
		return strings.TrimSpace(o.Region) != "" || strings.TrimSpace(o.Endpoint) != ""
	case "custom":
		return strings.TrimSpace(o.Endpoint) != ""
	}
	return false
}

// Masked는 화면에 돌려줄 사본이다.
func (o ObjectSettings) Masked() ObjectSettings {
	out := o
	out.SecretKey = maskToken(o.SecretKey)
	return out
}

// maskToken은 비밀 값의 앞뒤 두 글자만 남긴다. 무엇이 저장되어 있는지 알아볼 만큼만.
func maskToken(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return "••••"
	}
	return v[:2] + "••••" + v[len(v)-2:]
}

// D1Settings는 저장된 D1 설정을 읽는다. 없으면 기본값이다.
func (s *Store) D1Settings(ctx context.Context) (*D1Settings, error) {
	out := &D1Settings{IntervalSec: D1IntervalDefault}
	var stored struct {
		D1Settings
		TokenEnc string `json:"tokenEnc,omitempty"`
	}
	ok, err := s.readSealedSetting(ctx, SettingCloudD1, &stored, &out.UpdatedAt, &out.UpdatedBy)
	if err != nil || !ok {
		return out, err
	}
	at, by := out.UpdatedAt, out.UpdatedBy
	*out = stored.D1Settings
	out.UpdatedAt, out.UpdatedBy = at, by
	if out.APIToken, err = s.secret.Open(stored.TokenEnc); err != nil {
		return nil, fmt.Errorf("D1 API 토큰을 풀지 못했습니다(마스터 키가 바뀌었을 수 있습니다): %w", err)
	}
	if out.IntervalSec <= 0 {
		out.IntervalSec = D1IntervalDefault
	}
	return out, nil
}

// SaveD1Settings는 D1 설정을 저장한다. 토큰은 봉인해 넣는다.
func (s *Store) SaveD1Settings(ctx context.Context, in D1Settings, actorID string) error {
	enc, err := s.secret.Seal(strings.TrimSpace(in.APIToken))
	if err != nil {
		return err
	}
	payload := struct {
		D1Settings
		TokenEnc string `json:"tokenEnc,omitempty"`
	}{D1Settings: in, TokenEnc: enc}
	payload.APIToken = ""
	payload.UpdatedAt, payload.UpdatedBy = nil, ""
	return s.writeSetting(ctx, SettingCloudD1, payload, actorID)
}

// ObjectSettings는 저장된 오브젝트 스토리지 설정을 읽는다. 없으면 기본값이다.
func (s *Store) ObjectSettings(ctx context.Context) (*ObjectSettings, error) {
	out := &ObjectSettings{Provider: "r2", Prefix: "dbstudio/", Encrypt: true,
		EveryHours: 24, At: "03:00", Keep: 14}
	var stored struct {
		ObjectSettings
		SecretEnc string `json:"secretEnc,omitempty"`
	}
	stored.ObjectSettings = *out
	ok, err := s.readSealedSetting(ctx, SettingCloudObject, &stored, &out.UpdatedAt, &out.UpdatedBy)
	if err != nil || !ok {
		return out, err
	}
	at, by := out.UpdatedAt, out.UpdatedBy
	*out = stored.ObjectSettings
	out.UpdatedAt, out.UpdatedBy = at, by
	if out.SecretKey, err = s.secret.Open(stored.SecretEnc); err != nil {
		return nil, fmt.Errorf("오브젝트 스토리지 시크릿 키를 풀지 못했습니다(마스터 키가 바뀌었을 수 있습니다): %w", err)
	}
	return out, nil
}

// SaveObjectSettings는 오브젝트 스토리지 설정을 저장한다. 시크릿 키는 봉인해 넣는다.
func (s *Store) SaveObjectSettings(ctx context.Context, in ObjectSettings, actorID string) error {
	enc, err := s.secret.Seal(strings.TrimSpace(in.SecretKey))
	if err != nil {
		return err
	}
	payload := struct {
		ObjectSettings
		SecretEnc string `json:"secretEnc,omitempty"`
	}{ObjectSettings: in, SecretEnc: enc}
	payload.SecretKey = ""
	payload.UpdatedAt, payload.UpdatedBy = nil, ""
	return s.writeSetting(ctx, SettingCloudObject, payload, actorID)
}

// readSealedSetting은 JSON 설정 하나를 dst에 읽는다. 없으면 false다.
func (s *Store) readSealedSetting(ctx context.Context, key string, dst any,
	updatedAt **time.Time, updatedBy *string) (bool, error) {
	var raw string
	var at, by sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT value, updated_at, updated_by FROM app_settings WHERE key = ?`, key).Scan(&raw, &at, &by)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read setting %s: %w", key, err)
	}
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		// 값이 깨졌으면 설정이 없는 것으로 본다. 앱이 멈출 이유는 아니다.
		return false, nil
	}
	*updatedAt = parseTimePtr(at)
	*updatedBy = by.String
	return true, nil
}

func (s *Store) writeSetting(ctx context.Context, key string, v any, actorID string) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, key, string(b), actorID)
}
