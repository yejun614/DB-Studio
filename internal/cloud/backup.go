package cloud

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/store"
)

// 오브젝트 스토리지(R2·S3) 백업.
//
// 백업하는 것은 DB Studio 자신의 메타 DB 하나다(커넥션·권한·ERD·매크로·감사 기록 전부가 여기
// 있다). 대상 DB의 논리 덤프(백업 화면)와는 다른 것이다 — 그쪽은 "관리하는 DB"의 사본이고,
// 이쪽은 "관리하는 앱"의 사본이다.
//
// 마스터 키는 올리지 않는다. 키와 그 키로 봉인한 것을 같은 곳에 두면 봉인한 의미가 없다.
// 그래서 복구하려면 원래 서버와 같은 키로 띄운 DB Studio 가 필요하다.

const (
	backupPrefix = "dbstudio-"
	backupExt    = ".db.gz"
	encExt       = ".enc"
	backupLabel  = "dbstudio/backup/v1"
)

// Backup은 버킷에 있는 백업 하나다.
type Backup struct {
	Key        string    `json:"key"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Encrypted  bool      `json:"encrypted"`
}

func isBackupKey(key string) bool {
	base := path.Base(key)
	return strings.HasPrefix(base, backupPrefix) && strings.Contains(base, backupExt)
}

func (s *Service) objectClient(ctx context.Context) (*ObjectStore, *store.ObjectSettings, error) {
	cfg, err := s.st.ObjectSettings(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.Ready() {
		return nil, cfg, errors.New("버킷·액세스 키·시크릿 키(와 R2 계정 ID 또는 엔드포인트)를 먼저 저장하세요")
	}
	o, err := NewObjectStore(*cfg)
	if err != nil {
		return nil, cfg, err
	}
	return o, cfg, nil
}

// TestObject는 저장된 설정으로 버킷을 나열해 본다. 백업 수와 올릴 위치를 돌려준다.
func (s *Service) TestObject(ctx context.Context) (string, int, error) {
	o, _, err := s.objectClient(ctx)
	if err != nil {
		return "", 0, err
	}
	list, err := s.listBackups(ctx, o)
	if err != nil {
		return "", 0, err
	}
	return o.Describe(), len(list), nil
}

// ListBackups는 버킷에 있는 백업을 새것부터 돌려준다.
func (s *Service) ListBackups(ctx context.Context) ([]Backup, error) {
	o, _, err := s.objectClient(ctx)
	if err != nil {
		return nil, err
	}
	return s.listBackups(ctx, o)
}

func (s *Service) listBackups(ctx context.Context, o *ObjectStore) ([]Backup, error) {
	objs, err := o.List(ctx, 5000)
	if err != nil {
		return nil, err
	}
	out := []Backup{}
	for _, ob := range objs {
		if !isBackupKey(ob.Key) {
			continue
		}
		out = append(out, Backup{
			Key: ob.Key, Name: path.Base(ob.Key), Size: ob.Size, ModifiedAt: ob.ModifiedAt,
			Encrypted: strings.HasSuffix(ob.Key, encExt),
		})
	}
	return out, nil
}

// StartBackup은 지금 백업한다.
func (s *Service) StartBackup() (*Job, error) {
	return s.startJob("object.backup", "오브젝트 스토리지로 백업", func(ctx context.Context, j *Job) (string, error) {
		key, size, err := s.backup(ctx, j)
		s.recordBackup(ctx, key, err)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s 를 올렸습니다 (%s)", path.Base(key), humanSize(size)), nil
	})
}

func (s *Service) recordBackup(ctx context.Context, key string, err error) {
	now := time.Now()
	_ = s.st.SetLocalStateTime(ctx, store.StateObjLastAttempt, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.obj.LastError = firstLine(err)
		s.obj.ErrorAt = &now
		return
	}
	_ = s.st.SetLocalStateTime(ctx, store.StateObjLastBackup, now)
	s.obj.LastKey = key
	s.obj.LastError, s.obj.ErrorAt = "", nil
}

func (s *Service) backup(ctx context.Context, j *Job) (string, int64, error) {
	o, cfg, err := s.objectClient(ctx)
	if err != nil {
		return "", 0, err
	}
	s.stage(j, "메타 DB 스냅샷", 0)
	snap, err := s.tempPath("backup.db")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(snap)
	if err := s.st.SnapshotTo(ctx, snap); err != nil {
		return "", 0, err
	}

	s.stage(j, "압축·암호화", 0)
	packed, err := s.tempPath("backup.gz")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(packed)
	sum, size, err := s.pack(snap, packed, cfg.Encrypt)
	if err != nil {
		return "", 0, err
	}

	now := time.Now().UTC()
	name := backupPrefix + now.Format("20060102T150405Z") + backupExt
	if cfg.Encrypt {
		name += encExt
	}
	key := o.Prefix() + name
	schema, _ := s.st.SchemaVersion(ctx)
	s.stage(j, "올리기", size)
	if err := o.Put(ctx, key, packed, sum, map[string]string{
		"dbstudio-schema":    strconv.Itoa(schema),
		"dbstudio-version":   appVersion(),
		"dbstudio-key-fp":    s.st.Secret().Fingerprint(),
		"dbstudio-encrypted": strconv.FormatBool(cfg.Encrypt),
		"dbstudio-node":      s.opt.NodeName,
	}); err != nil {
		return "", 0, err
	}
	if cfg.Keep > 0 {
		s.stage(j, "오래된 백업 정리", size)
		if n, err := s.prune(ctx, o, cfg.Keep); err != nil {
			// 정리 실패는 백업 실패가 아니다. 올린 것은 올라갔다.
			s.log.Warn("오래된 백업을 지우지 못했습니다", "err", err)
		} else if n > 0 {
			s.log.Info("보존 개수를 넘은 백업을 지웠습니다", "count", n)
		}
	}
	return key, size, nil
}

// pack은 스냅샷을 gzip으로 묶고(필요하면 암호화해) dst에 쓴다. SHA-256과 크기를 돌려준다.
func (s *Service) pack(src, dst string, encrypt bool) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", 0, err
	}
	defer out.Close()

	h := sha256.New()
	counter := &countWriter{}
	var sink io.Writer = io.MultiWriter(out, h, counter)
	var sealer io.WriteCloser
	if encrypt {
		sealer, err = NewSealWriter(sink, s.st.Secret().DeriveKey(backupLabel))
		if err != nil {
			return "", 0, err
		}
		sink = sealer
	}
	gz, err := gzip.NewWriterLevel(sink, gzip.BestCompression)
	if err != nil {
		return "", 0, err
	}
	if _, err := io.Copy(gz, in); err != nil {
		return "", 0, err
	}
	if err := gz.Close(); err != nil {
		return "", 0, err
	}
	if sealer != nil {
		if err := sealer.Close(); err != nil {
			return "", 0, err
		}
	}
	if err := out.Sync(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), counter.n, nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// prune은 보존 개수를 넘은 **이 앱의** 백업만 지운다. 이름 규칙이 다른 객체는 건드리지 않는다.
func (s *Service) prune(ctx context.Context, o *ObjectStore, keep int) (int, error) {
	list, err := s.listBackups(ctx, o)
	if err != nil {
		return 0, err
	}
	// 이름에 시각이 들어 있어 이름 역순이 곧 새것부터다.
	removed := 0
	for i := keep; i < len(list); i++ {
		if err := o.Delete(ctx, list[i].Key); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// StartRestore는 고른 백업으로 이 서버의 메타 DB를 되돌린다. after는 StartPull과 같다.
func (s *Service) StartRestore(key string, after func(context.Context)) (*Job, error) {
	key = strings.TrimSpace(key)
	if !isBackupKey(key) {
		return nil, errors.New("DB Studio 백업 파일이 아닙니다")
	}
	return s.startJob("object.restore", "백업으로 복구", func(ctx context.Context, j *Job) (string, error) {
		if err := s.restore(ctx, key, j); err != nil {
			return "", err
		}
		if after != nil {
			after(ctx)
		}
		j.Relogin = true
		return path.Base(key) + " 로 되돌렸습니다. 다시 로그인하세요", nil
	})
}

func (s *Service) restore(ctx context.Context, key string, j *Job) error {
	o, _, err := s.objectClient(ctx)
	if err != nil {
		return err
	}
	s.stage(j, "내려받기", 0)
	body, meta, err := o.Get(ctx, key)
	if err != nil {
		return err
	}
	defer body.Close()
	if fp := meta["dbstudio-key-fp"]; fp != "" && fp != s.st.Secret().Fingerprint() {
		return errors.New("이 백업은 다른 마스터 키를 쓰는 서버에서 만들었습니다. 원래 서버와 같은 키로 " +
			"이 서버를 띄운 뒤 복구하세요 — 그러지 않으면 저장된 DB 자격증명을 하나도 풀 수 없습니다")
	}

	raw, err := s.tempPath("restore.gz")
	if err != nil {
		return err
	}
	defer os.Remove(raw)
	f, err := os.Create(raw)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, &progressReader{r: body, onRead: func(n int64) { s.stage(j, "내려받기", n) }})
	f.Close()
	if err != nil {
		return fmt.Errorf("백업을 내려받지 못했습니다: %w", err)
	}

	s.stage(j, "풀기·검사", n)
	dbPath, err := s.tempPath("restore.db")
	if err != nil {
		return err
	}
	defer os.Remove(dbPath)
	encrypted := strings.HasSuffix(key, encExt) || meta["dbstudio-encrypted"] == "true"
	if err := s.unpack(raw, dbPath, encrypted); err != nil {
		return err
	}
	if err := s.checkBackupFile(ctx, dbPath); err != nil {
		return err
	}

	s.stage(j, "덮어쓰기 전 안전 사본", n)
	safety, err := s.safetyCopy(ctx, "restore")
	if err != nil {
		return fmt.Errorf("덮어쓰기 전 안전 사본을 만들지 못해 멈췄습니다: %w", err)
	}
	restoreSettings, err := s.keepCloudSettings(ctx)
	if err != nil {
		return err
	}
	s.stage(j, "이 서버에 적용", n)
	if err := s.st.RestoreFromFile(ctx, dbPath); err != nil {
		return fmt.Errorf("백업을 적용하지 못했습니다(이 서버는 바뀌지 않았습니다): %w", err)
	}
	if err := restoreSettings(ctx); err != nil {
		s.log.Warn("클라우드 설정을 되살리지 못했습니다", "err", err)
	}
	s.log.Info("오브젝트 스토리지 백업으로 메타 DB 를 되돌렸습니다", "key", key, "safetyCopy", safety)

	// D1 과 이어 쓰던 중이면 원격도 되돌린 상태로 맞춘다. 행 단위 변경으로 따라가게 두면
	// 표 전체가 지워지고 다시 들어간 기록이 하나씩 올라간다 — 전체를 다시 올리는 편이 짧다.
	if _, synced, _ := s.st.LocalStateInt(ctx, store.StateD1PushedSeq); synced {
		_ = s.st.SetLocalState(ctx, store.StateD1FullNeeded, "restore")
		s.Kick()
	}
	s.afterReplace("백업으로 되돌려 이 서버의 데이터가 바뀌었습니다. 다시 여세요")
	return nil
}

func (s *Service) unpack(src, dst string, encrypted bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	var r io.Reader = in
	if encrypted {
		r, err = NewOpenReader(in, s.st.Secret().DeriveKey(backupLabel))
		if err != nil {
			return err
		}
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("백업 파일을 풀지 못했습니다: %w", err)
	}
	defer gz.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, gz); err != nil {
		out.Close()
		return fmt.Errorf("백업 파일을 풀지 못했습니다: %w", err)
	}
	return out.Close()
}

// checkBackupFile은 풀어 낸 파일이 이 서버에 넣어도 되는 메타 DB인지 본다.
func (s *Service) checkBackupFile(ctx context.Context, dbPath string) error {
	db, err := sql.Open("sqlite", strings.ReplaceAll(dbPath, "\\", "/")+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return fmt.Errorf("SQLite 파일이 아닙니다: %w", err)
	}
	if check != "ok" {
		return fmt.Errorf("백업 파일이 손상됐습니다: %s", check)
	}
	var version sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		return errors.New("DB Studio 메타 DB 가 아닙니다")
	}
	local, _ := s.st.SchemaVersion(ctx)
	if int(version.Int64) > local {
		return fmt.Errorf("이 백업은 더 새 버전의 DB Studio 가 만들었습니다(스키마 %d > %d). 이 서버를 먼저 올리세요",
			version.Int64, local)
	}
	return nil
}

type progressReader struct {
	r      io.Reader
	n      int64
	last   int64
	onRead func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.n-p.last >= 1<<20 {
		p.last = p.n
		p.onRead(p.n)
	}
	return n, err
}

// ---------- 정기 백업 ----------

// nextBackupAt은 다음 정기 백업 시각이다.
//
// 기준점(At, 서버 시각)에서 EveryHours 간격으로 늘어선 시각 중, 마지막 시도 뒤의 첫 번째다.
// 서버가 꺼져 있어 놓친 시각이 있으면 다음 시각은 과거가 되고, 켜지자마자 한 번 따라잡는다
// (놓친 횟수만큼 여러 번 하지는 않는다 — 같은 상태를 연달아 올릴 뿐이다).
func (s *Service) nextBackupAt(ctx context.Context, cfg *store.ObjectSettings) time.Time {
	last, _ := s.st.LocalStateTime(ctx, store.StateObjLastAttempt)
	if ok, _ := s.st.LocalStateTime(ctx, store.StateObjLastBackup); ok.After(last) {
		last = ok
	}
	return NextRun(cfg.At, cfg.EveryHours, last, time.Now())
}

// NextRun은 정기 실행 시각을 계산한다. last가 영값이면 now 뒤의 첫 시각이다.
func NextRun(at string, everyHours int, last, now time.Time) time.Time {
	if everyHours < 1 {
		everyHours = 24
	}
	every := time.Duration(everyHours) * time.Hour
	hh, mm := 3, 0
	if h, m, ok := strings.Cut(strings.TrimSpace(at), ":"); ok {
		if v, err := strconv.Atoi(h); err == nil && v >= 0 && v < 24 {
			hh = v
		}
		if v, err := strconv.Atoi(m); err == nil && v >= 0 && v < 60 {
			mm = v
		}
	}
	base := last
	if base.IsZero() {
		base = now
	}
	local := base.In(now.Location())
	anchor := time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, now.Location())
	diff := base.Sub(anchor)
	k := diff / every
	if diff >= 0 {
		k++
	}
	next := anchor.Add(k * every)
	for !next.After(base) {
		next = next.Add(every)
	}
	return next
}

// tickBackup은 정기 백업 시각이 되었으면 한 번 백업한다(1분에 한 번만 본다).
func (s *Service) tickBackup(ctx context.Context) {
	s.mu.Lock()
	if time.Since(s.lastSchedAt) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.lastSchedAt = time.Now()
	s.mu.Unlock()

	cfg, err := s.st.ObjectSettings(ctx)
	if err != nil || !cfg.ScheduleEnabled || !cfg.Ready() {
		return
	}
	if time.Now().Before(s.nextBackupAt(ctx, cfg)) {
		return
	}
	// 정기 백업도 같은 작업 칸을 쓴다. 그래야 사람이 누른 복구와 겹치지 않고, 화면에 "지금
	// 백업 중"이 보인다.
	if _, err := s.startJob("object.backup", "정기 백업", func(ctx context.Context, j *Job) (string, error) {
		key, size, err := s.backup(ctx, j)
		s.recordBackup(ctx, key, err)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s 를 올렸습니다 (%s)", path.Base(key), humanSize(size)), nil
	}); err != nil && !errors.Is(err, ErrBusy) {
		s.log.Warn("정기 백업을 시작하지 못했습니다", "err", err)
	}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
