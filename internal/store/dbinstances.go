package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DB Studio 가 도커로 만든 DB 인스턴스.
//
// 도커가 진짜 상태를 들고 있고 우리는 **설정과 비밀과 기록**을 들고 있다.
// 그 구분이 이 파일의 규칙을 정한다: status 는 캐시라서 언제든 도커의 값으로
// 덮어써도 되고, values 와 비밀은 도커에 없으므로 우리가 잃으면 끝이다.

// 인스턴스 상태.
const (
	InstanceCreating  = "creating"
	InstanceRunning   = "running"
	InstanceStopped   = "stopped"
	InstanceUnhealthy = "unhealthy"
	InstanceFailed    = "failed"
	InstanceRemoved   = "removed"
)

// DBInstance는 만든 DB 하나다.
type DBInstance struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Image     string `json:"image"`
	Version   string `json:"version"`

	ContainerID   string `json:"containerId,omitempty"`
	ContainerName string `json:"containerName"`
	VolumeName    string `json:"volumeName,omitempty"`

	HostPort int `json:"hostPort"`
	Port     int `json:"port"`

	// Values는 사람이 고른 설정이다. 비밀은 여기 없다.
	Values map[string]string `json:"values"`

	Status   string `json:"status"`
	Health   string `json:"health,omitempty"`
	Error    string `json:"error,omitempty"`
	Progress string `json:"progress,omitempty"`

	ConnectionID  string    `json:"connectionId,omitempty"`
	CreatedBy     string    `json:"createdBy,omitempty"`
	CreatedByName string    `json:"createdByName,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

var (
	// ErrInstanceNameTaken은 같은 이름이 이미 있다는 뜻이다.
	ErrInstanceNameTaken = errors.New("이미 같은 이름의 DB 컨테이너가 있습니다")
)

const dbInstanceSelect = `
	SELECT id, project_id, name, kind, image, version,
	       container_id, container_name, volume_name,
	       host_port, port, values_json,
	       status, health, error, progress,
	       COALESCE(connection_id, ''), COALESCE(created_by, ''), created_by_name,
	       created_at, updated_at
	FROM db_instances`

func scanDBInstance(row interface{ Scan(...any) error }) (*DBInstance, error) {
	var in DBInstance
	var valuesJSON, createdAt, updatedAt string
	err := row.Scan(&in.ID, &in.ProjectID, &in.Name, &in.Kind, &in.Image, &in.Version,
		&in.ContainerID, &in.ContainerName, &in.VolumeName,
		&in.HostPort, &in.Port, &valuesJSON,
		&in.Status, &in.Health, &in.Error, &in.Progress,
		&in.ConnectionID, &in.CreatedBy, &in.CreatedByName,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	in.Values = map[string]string{}
	if valuesJSON != "" {
		// 못 읽어도 인스턴스 자체는 돌려준다. 설정을 잃는 것보다 목록이 통째로
		// 안 나오는 것이 나쁘다 — 목록이 없으면 멈추거나 지울 수도 없다.
		_ = json.Unmarshal([]byte(valuesJSON), &in.Values)
	}
	in.CreatedAt = parseTime(createdAt)
	in.UpdatedAt = parseTime(updatedAt)
	return &in, nil
}

// CreateDBInstanceParams는 인스턴스를 적어 둘 때 주는 값이다.
type CreateDBInstanceParams struct {
	ProjectID     string
	Name          string
	Kind          string
	Image         string
	Version       string
	ContainerName string
	VolumeName    string
	Port          int
	HostPort      int
	Values        map[string]string
	// Secrets는 봉해서 따로 둘 값이다(비밀번호 등).
	Secrets map[string]string
	ActorID string
	Actor   string
}

// CreateDBInstance는 만들기 **전에** 줄을 적는다.
//
// 왜 먼저 적는가: 이미지를 내려받는 데 몇 분이 걸린다. 그 사이에 화면이
// 보여줄 것이 있어야 하고(진행률), 앱이 죽었다가 살아났을 때 "만들다 만 것"이
// 남아 있어야 한다 — 도커에만 있으면 우리는 그것을 우리 것으로 알아볼 수 없다.
func (s *Store) CreateDBInstance(ctx context.Context, p CreateDBInstanceParams) (*DBInstance, error) {
	if p.ProjectID == "" {
		return nil, ErrNoProject
	}
	valuesJSON, err := json.Marshal(p.Values)
	if err != nil {
		return nil, fmt.Errorf("marshal values: %w", err)
	}
	sealed, err := s.sealInstanceSecrets(p.Secrets)
	if err != nil {
		return nil, err
	}

	id := "dbi_" + uuid.NewString()
	now := nowString()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO db_instances (id, project_id, name, kind, image, version,
			container_name, volume_name, host_port, port, values_json,
			status, created_by, created_by_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.ProjectID, p.Name, p.Kind, p.Image, p.Version,
		p.ContainerName, p.VolumeName, p.HostPort, p.Port, string(valuesJSON),
		InstanceCreating, nullString(p.ActorID), p.Actor, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrInstanceNameTaken
		}
		return nil, fmt.Errorf("insert db_instance: %w", err)
	}
	if sealed != "" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO db_instance_secrets (instance_id, secrets_enc, updated_at)
			VALUES (?, ?, ?)`, id, sealed, now)
		if err != nil {
			return nil, fmt.Errorf("insert db_instance_secrets: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return s.GetDBInstance(ctx, id)
}

// GetDBInstance는 하나를 읽는다.
func (s *Store) GetDBInstance(ctx context.Context, id string) (*DBInstance, error) {
	in, err := scanDBInstance(s.db.QueryRowContext(ctx, dbInstanceSelect+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get db_instance: %w", err)
	}
	return in, nil
}

// ListDBInstances는 프로젝트의 인스턴스를 이름 순으로 읽는다.
//
// projectID 가 비면 전부 읽는다(슈퍼 어드민의 전체 보기).
func (s *Store) ListDBInstances(ctx context.Context, projectID string) ([]*DBInstance, error) {
	query := dbInstanceSelect
	args := []any{}
	if projectID != "" {
		query += " WHERE project_id = ?"
		args = append(args, projectID)
	}
	// 지운 것은 뒤로 보낸다. 목록의 앞은 지금 쓰는 것들이어야 한다.
	query += " ORDER BY (status = 'removed'), name"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list db_instances: %w", err)
	}
	defer rows.Close()

	out := []*DBInstance{}
	for rows.Next() {
		in, err := scanDBInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("scan db_instance: %w", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate db_instances: %w", err)
	}
	return out, nil
}

// UpdateDBInstanceParams는 고칠 값들이다. nil 은 "그대로 둔다".
//
// 포인터로 두는 이유: 진행률만 고치는 호출과 상태만 고치는 호출이 따로 있고,
// 값으로 받으면 안 준 필드가 빈 문자열로 덮인다 — 그러면 진행률을 적는 사이에
// 오류 메시지가 지워진다.
type UpdateDBInstanceParams struct {
	ContainerID  *string
	HostPort     *int
	Status       *string
	Health       *string
	Error        *string
	Progress     *string
	ConnectionID *string
}

// UpdateDBInstance는 인스턴스의 상태를 고친다.
func (s *Store) UpdateDBInstance(ctx context.Context, id string, p UpdateDBInstanceParams) error {
	sets := []string{"updated_at = ?"}
	args := []any{nowString()}

	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if p.ContainerID != nil {
		add("container_id", *p.ContainerID)
	}
	if p.HostPort != nil {
		add("host_port", *p.HostPort)
	}
	if p.Status != nil {
		add("status", *p.Status)
	}
	if p.Health != nil {
		add("health", *p.Health)
	}
	if p.Error != nil {
		add("error", *p.Error)
	}
	if p.Progress != nil {
		add("progress", *p.Progress)
	}
	if p.ConnectionID != nil {
		// 빈 문자열은 "연결 없음"이다. NULL 로 넣어야 외래키가 걸리지 않는다.
		add("connection_id", nullString(*p.ConnectionID))
	}
	if len(sets) == 1 {
		return nil // 고칠 것이 없다
	}

	args = append(args, id)
	res, err := s.db.ExecContext(ctx,
		"UPDATE db_instances SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return fmt.Errorf("update db_instance: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDBInstance는 기록까지 지운다.
//
// 컨테이너를 지우는 것과 다른 일이다. 컨테이너만 지우고 기록을 남기면 목록에
// "지움" 으로 남고, 그 줄을 치우는 것이 이 함수다.
func (s *Store) DeleteDBInstance(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM db_instances WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete db_instance: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DBInstanceSecrets는 봉해 둔 값을 꺼낸다.
//
// 따로 부르게 한 이유: 목록을 읽는 길에 비밀이 함께 오지 않아야 한다.
// 같은 함수로 주면 목록 한 번에 모든 비밀번호가 메모리로 올라오고, 그것을
// 그대로 JSON 으로 내보내는 길이 생긴다.
func (s *Store) DBInstanceSecrets(ctx context.Context, id string) (map[string]string, error) {
	var enc string
	err := s.db.QueryRowContext(ctx,
		"SELECT secrets_enc FROM db_instance_secrets WHERE instance_id = ?", id).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get db_instance_secrets: %w", err)
	}
	return s.openInstanceSecrets(enc)
}

// SaveDBInstanceSecrets는 봉해 둔 값을 바꾼다.
func (s *Store) SaveDBInstanceSecrets(ctx context.Context, id string, secrets map[string]string) error {
	sealed, err := s.sealInstanceSecrets(secrets)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO db_instance_secrets (instance_id, secrets_enc, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (instance_id) DO UPDATE SET
			secrets_enc = excluded.secrets_enc, updated_at = excluded.updated_at`,
		id, sealed, nowString())
	if err != nil {
		return fmt.Errorf("save db_instance_secrets: %w", err)
	}
	return nil
}

func (s *Store) sealInstanceSecrets(secrets map[string]string) (string, error) {
	if len(secrets) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(secrets)
	if err != nil {
		return "", fmt.Errorf("marshal secrets: %w", err)
	}
	sealed, err := s.secret.Seal(string(raw))
	if err != nil {
		return "", fmt.Errorf("seal secrets: %w", err)
	}
	return sealed, nil
}

func (s *Store) openInstanceSecrets(enc string) (map[string]string, error) {
	out := map[string]string{}
	if enc == "" {
		return out, nil
	}
	raw, err := s.secret.Open(enc)
	if err != nil {
		return nil, fmt.Errorf("open secrets: %w", err)
	}
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("unmarshal secrets: %w", err)
	}
	return out, nil
}
