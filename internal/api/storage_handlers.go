package api

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/storage"
	"dbstudio/internal/store"
)

// 분산 스토리지(하둡·Ceph) 관리 API.
//
// 권한은 DB와 같은 규칙을 쓴다. 커넥션 등급(monitor/read/write…)이 이미 "누가 이 대상을
// 볼 수 있는가"를 정하고 있으므로, 스토리지라고 해서 별도의 권한 체계를 만들 이유가 없다.
//   - 조회(개요·목록·탐색): 모니터링 등급 이상
//   - 변경(디렉터리 만들기·이름 바꾸기·삭제): data.write 능력
//
// 삭제가 특히 무거운 이유: HDFS의 recursive 삭제는 디렉터리 아래 전부를 지우고 되돌릴
// 방법이 없다(휴지통이 꺼져 있으면 즉시 사라진다). 그래서 지우기 전에 무엇이 얼마나
// 사라지는지 세어 주고, 그 숫자를 화면이 확인 대화상자에 그대로 보여준다.

// resolveStorage는 커넥션을 스토리지 클라이언트로 만든다.
func (s *Server) resolveStorage(c *fiber.Ctx, level model.Level) (*model.Connection, dbx.Target, error) {
	var t dbx.Target
	id := c.Params("id")
	conn, err := s.st.GetConnection(c.Context(), id)
	if err != nil {
		return nil, t, err
	}
	// 여기서 fail()을 쓰지 않는 이유: fail은 응답을 쓰고 **nil을 돌려준다**(Fiber의 관례다).
	// 헬퍼에서 그 값을 그대로 반환하면 호출부의 `err != nil` 검사가 통과해 버려,
	// 거절해 놓고도 핸들러가 계속 실행된다.
	if !conn.Kind.IsStorage() {
		return nil, t, fiber.NewError(fiber.StatusBadRequest, "이 커넥션은 스토리지 클러스터가 아닙니다")
	}
	d, err := s.requireLevel(c, conn.ID, level)
	if err != nil {
		return nil, t, err
	}
	if !d.Allowed {
		return nil, t, fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	if !conn.Enabled {
		return nil, t, fiber.NewError(fiber.StatusConflict, "비활성 상태인 커넥션입니다")
	}
	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return nil, t, err
	}
	return conn, dbx.Target{Conn: conn, Secret: secret}, nil
}

func hadoopClient(t dbx.Target) *storage.Hadoop {
	return storage.NewHadoop(storage.ConfigFrom(t.Conn, t.Secret, storage.HadoopDefaultPort))
}

func cephClient(t dbx.Target) *storage.Ceph {
	return storage.NewCeph(storage.ConfigFrom(t.Conn, t.Secret, storage.CephDefaultPort))
}

func s3Client(t dbx.Target) *storage.S3 {
	return storage.NewS3(storage.ConfigFrom(t.Conn, t.Secret, storage.S3DefaultPort))
}

// handleStorageOverview는 클러스터 개요 한 장이다.
func (s *Server) handleStorageOverview(c *fiber.Ctx) error {
	conn, t, err := s.resolveStorage(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	var ov *storage.Overview
	switch conn.Kind {
	case model.KindHadoop:
		ov, err = hadoopClient(t).Overview(c.Context())
	case model.KindCeph:
		ov, err = cephClient(t).Overview(c.Context())
	case model.KindS3:
		ov, err = s3Client(t).Overview(c.Context())
	}
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "storage_unreachable",
			"클러스터 상태를 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{
		"overview": ov,
		"kind":     conn.Kind,
		// 화면이 어떤 탭을 그릴지 정하는 근거다. 종류 이름으로 화면에서 분기하면
		// 종류가 늘 때마다 화면을 고쳐야 한다.
		"features": storageFeatures(conn.Kind),
	})
}

// storageFeatures는 이 종류에서 쓸 수 있는 기능이다.
func storageFeatures(kind model.DBKind) fiber.Map {
	switch kind {
	case model.KindHadoop:
		return fiber.Map{"browse": true, "apps": true, "pools": false, "osds": false,
			"buckets": false, "write": true}
	case model.KindCeph:
		// 쓰기가 false인 이유는 storage/ceph.go의 주석에 있다(되돌릴 수 없는 조작이다).
		return fiber.Map{"browse": false, "apps": false, "pools": true, "osds": true,
			"buckets": true, "write": false}
	case model.KindS3:
		// 오브젝트 스토리지에는 버킷과 객체만 있다. 풀·OSD·경로는 다른 종류의
		// 개념이라 그 탭을 그리지 않는다.
		//
		// objects 가 buckets 와 따로 있는 이유: Ceph 도 버킷 목록은 주지만
		// 객체를 훑는 길은 없다(대시보드 API 에 그 자리가 없다).
		return fiber.Map{"browse": false, "apps": false, "pools": false, "osds": false,
			"buckets": true, "objects": true, "write": false}
	}
	return fiber.Map{}
}

// handleStorageBrowse는 HDFS 경로 목록이다.
func (s *Server) handleStorageBrowse(c *fiber.Ctx) error {
	conn, t, err := s.resolveStorage(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if conn.Kind != model.KindHadoop {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류는 경로 탐색을 지원하지 않습니다")
	}
	path := storage.CleanPath(c.Query("path", "/"))
	cl := hadoopClient(t)
	entries, err := cl.List(c.Context(), path)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "browse_failed", "목록을 읽지 못했습니다", err.Error())
	}
	out := fiber.Map{"path": path, "entries": entries}
	// 요약(파일 수·용량·쿼터)은 실패해도 목록을 막지 않는다. 쿼터 조회 권한이 없는
	// 경로가 흔하고, 그때 목록까지 못 보면 탐색 자체가 불가능해진다.
	if sum, err := cl.Summary(c.Context(), path); err == nil {
		out["summary"] = sum
	} else {
		out["summaryNote"] = err.Error()
	}
	return c.JSON(out)
}

// handleStorageApps는 YARN 애플리케이션 목록이다.
func (s *Server) handleStorageApps(c *fiber.Ctx) error {
	conn, t, err := s.resolveStorage(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if conn.Kind != model.KindHadoop {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 애플리케이션 목록이 없습니다")
	}
	limit, _ := strconv.Atoi(c.Query("limit", "50"))
	apps, err := hadoopClient(t).Apps(c.Context(), c.Query("states", ""), limit)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "apps_failed",
			"애플리케이션 목록을 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"apps": apps})
}

// handleStoragePools는 Ceph 풀 목록이다.
func (s *Server) handleStoragePools(c *fiber.Ctx) error {
	conn, t, err := s.resolveStorage(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if conn.Kind != model.KindCeph {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 풀이 없습니다")
	}
	pools, err := cephClient(t).Pools(c.Context())
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "pools_failed", "풀 목록을 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"pools": pools})
}

// handleStorageOSDs는 Ceph OSD 목록이다.
func (s *Server) handleStorageOSDs(c *fiber.Ctx) error {
	conn, t, err := s.resolveStorage(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if conn.Kind != model.KindCeph {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 OSD가 없습니다")
	}
	osds, err := cephClient(t).OSDs(c.Context())
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "osds_failed", "OSD 목록을 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"osds": osds})
}

// handleStorageBuckets는 RGW 버킷 목록이다.
func (s *Server) handleStorageBuckets(c *fiber.Ctx) error {
	conn, t, err := s.resolveStorage(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	var (
		buckets []storage.Bucket
		note    string
	)
	switch conn.Kind {
	case model.KindCeph:
		buckets, note, err = cephClient(t).Buckets(c.Context())
	case model.KindS3:
		buckets, note, err = s3Client(t).Buckets(c.Context())
	default:
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 버킷이 없습니다")
	}
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "buckets_failed",
			"버킷 목록을 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"buckets": buckets, "note": note})
}

// handleStorageObjects는 S3 버킷 안의 객체 목록 한 장이다.
//
// 접두사로 접어서 보여준다(delimiter="/"). 수백만 개의 키를 평평하게 늘어놓으면
// 사람이 읽을 수 없고, 접두사로 접으면 파일 탐색기처럼 읽힌다 — 그것이 사람이
// 키 이름을 지을 때 실제로 의도한 구조다.
func (s *Server) handleStorageObjects(c *fiber.Ctx) error {
	conn, t, err := s.resolveStorage(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if !conn.Kind.IsObjectStore() {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 객체 목록이 없습니다")
	}
	bucket := strings.TrimSpace(c.Query("bucket"))
	if bucket == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "버킷을 고르세요")
	}
	page, err := s3Client(t).Objects(c.Context(), bucket,
		c.Query("prefix"), c.Query("cursor"))
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "objects_failed",
			"객체 목록을 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"page": page})
}

// handleStorageBucketStat은 버킷 하나의 크기를 어림잡는다.
//
// 따로 부르는 이유: S3 에는 이것을 묻는 API 가 없어 객체를 나열해 세는 수밖에
// 없다. 목록을 여는 값으로는 너무 크므로, 사람이 그 버킷을 골랐을 때만 센다.
func (s *Server) handleStorageBucketStat(c *fiber.Ctx) error {
	conn, t, err := s.resolveStorage(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if !conn.Kind.IsObjectStore() {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 버킷이 없습니다")
	}
	bucket := strings.TrimSpace(c.Query("bucket"))
	if bucket == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "버킷을 고르세요")
	}
	stat, err := s3Client(t).Stat(c.Context(), bucket)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "stat_failed",
			"버킷 크기를 재지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"stat": stat})
}

// storagePathRequest는 경로 조작 입력이다.
type storagePathRequest struct {
	Path      string `json:"path"`
	To        string `json:"to"`
	Recursive bool   `json:"recursive"`
}

// requireStorageWrite는 쓰기 능력을 확인한다.
func (s *Server) requireStorageWrite(c *fiber.Ctx, conn *model.Connection) error {
	if conn.Kind.IsObjectStore() {
		return fiber.NewError(fiber.StatusBadRequest,
			"오브젝트 스토리지는 조회 전용입니다. 객체 삭제는 되돌릴 수 없어 이 앱에서 실행하지 않습니다")
	}
	if conn.Kind == model.KindCeph {
		return fiber.NewError(fiber.StatusBadRequest,
			"Ceph는 조회 전용입니다. 풀·OSD 조작은 되돌릴 수 없어 이 앱에서 실행하지 않습니다")
	}
	d, err := s.requireCap(c, conn.ID, model.CapDataWrite)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	return nil
}

// handleStorageMkdir는 HDFS 디렉터리를 만든다.
func (s *Server) handleStorageMkdir(c *fiber.Ctx) error {
	conn, t, err := s.resolveStorage(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if err := s.requireStorageWrite(c, conn); err != nil {
		return err
	}
	var req storagePathRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	path := storage.CleanPath(req.Path)
	if path == "/" {
		return fail(c, fiber.StatusBadRequest, "bad_path", "만들 경로를 입력하세요")
	}
	if err := hadoopClient(t).Mkdir(c.Context(), path); err != nil {
		return failDetail(c, fiber.StatusBadGateway, "mkdir_failed", "디렉터리를 만들지 못했습니다", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: "storage.mkdir", TargetType: "connection", TargetID: conn.ID,
		Detail: map[string]any{"name": conn.Name, "path": path},
	})
	return c.JSON(fiber.Map{"ok": true, "path": path})
}

// handleStorageRename은 경로 이름을 바꾼다.
func (s *Server) handleStorageRename(c *fiber.Ctx) error {
	conn, t, err := s.resolveStorage(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if err := s.requireStorageWrite(c, conn); err != nil {
		return err
	}
	var req storagePathRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	from, to := storage.CleanPath(req.Path), storage.CleanPath(req.To)
	if from == "/" || to == "/" || from == to {
		return fail(c, fiber.StatusBadRequest, "bad_path", "바꿀 경로와 새 경로를 확인하세요")
	}
	if err := hadoopClient(t).Rename(c.Context(), from, to); err != nil {
		return failDetail(c, fiber.StatusBadGateway, "rename_failed", "이름을 바꾸지 못했습니다", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: "storage.rename", TargetType: "connection", TargetID: conn.ID,
		Detail: map[string]any{"name": conn.Name, "from": from, "to": to},
	})
	return c.JSON(fiber.Map{"ok": true, "path": to})
}

// handleStorageDelete는 경로를 지운다.
//
// dryRun을 두는 이유: 재귀 삭제는 무엇이 사라지는지 보지 않고 누르게 되는 대표적인
// 조작이다. 먼저 세어 보여주면, 지우려는 것이 정말 그것인지 확인할 기회가 생긴다.
func (s *Server) handleStorageDelete(c *fiber.Ctx) error {
	conn, t, err := s.resolveStorage(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if err := s.requireStorageWrite(c, conn); err != nil {
		return err
	}
	var req storagePathRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	path := storage.CleanPath(req.Path)
	if path == "/" {
		return fail(c, fiber.StatusBadRequest, "bad_path", "루트(/)는 지울 수 없습니다")
	}
	cl := hadoopClient(t)

	if c.Query("dryRun") == "1" {
		impact := fiber.Map{"path": path, "recursive": req.Recursive}
		if sum, err := cl.Summary(c.Context(), path); err == nil {
			impact["files"] = sum.Files
			impact["directories"] = sum.Directories
			impact["length"] = sum.Length
		} else {
			impact["note"] = err.Error()
		}
		return c.JSON(impact)
	}

	if err := cl.Delete(c.Context(), path, req.Recursive); err != nil {
		return failDetail(c, fiber.StatusBadGateway, "delete_failed", "지우지 못했습니다", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: "storage.delete", TargetType: "connection", TargetID: conn.ID,
		Detail: map[string]any{"name": conn.Name, "path": path, "recursive": req.Recursive},
	})
	return c.JSON(fiber.Map{"ok": true})
}
