package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"dbstudio/internal/model"
)

// DB 서버 저장소.
//
// 서버는 접속 정보와 자격증명을 갖고, 커넥션(관리 대상 DB)이 그 아래에 달린다.
// 자격증명이 여기 한 벌만 있다는 것이 이 구조의 요점이다.

const serverColumns = `s.id, s.project_id, COALESCE(pj.name, ''),
	s.name, s.kind, s.host, s.port, s.options,
	s.default_environment, s.tags, s.note, s.enabled,
	s.created_by, s.created_at, s.updated_at,
	COALESCE(sec.username, ''),
	(SELECT COUNT(*) FROM connections c WHERE c.server_id = s.id)`

const serverFrom = ` FROM servers s
	LEFT JOIN server_secrets sec ON sec.server_id = s.id
	LEFT JOIN projects pj ON pj.id = s.project_id`

func scanServer(row interface{ Scan(...any) error }) (*model.Server, error) {
	var v model.Server
	var options, tags, createdAt, updatedAt string
	var createdBy sql.NullString
	var enabled int
	if err := row.Scan(&v.ID, &v.ProjectID, &v.ProjectName,
		&v.Name, &v.Kind, &v.Host, &v.Port, &options,
		&v.DefaultEnvironment, &tags, &v.Note, &enabled,
		&createdBy, &createdAt, &updatedAt, &v.Username, &v.DatabaseCount); err != nil {
		return nil, err
	}
	v.Options = model.UnmarshalOptions(options)
	v.Tags = model.TagsFromString(tags)
	v.Enabled = enabled != 0
	v.CreatedBy = createdBy.String
	v.CreatedAt = parseTime(createdAt)
	v.UpdatedAt = parseTime(updatedAt)
	return &v, nil
}

// SaveServerParams는 서버 생성/수정 입력이다.
// Password가 nil이면 기존 비밀번호를 유지한다(수정 시).
type SaveServerParams struct {
	// ProjectID는 이 서버가 속할 프로젝트다. 만들 때 반드시 있어야 한다.
	ProjectID          string
	Name               string
	Kind               model.DBKind
	Host               string
	Port               int
	Options            model.Options
	DefaultEnvironment model.Environment
	Tags               []string
	Note               string
	Enabled            bool
	Username           string
	Password           *string
	Extra              map[string]string
	ActorID            string
}

func (s *Store) CreateServer(ctx context.Context, p SaveServerParams) (*model.Server, error) {
	id := "srv_" + uuid.NewString()
	now := nowString()
	optJSON, err := p.Options.MarshalDB()
	if err != nil {
		return nil, err
	}
	pw := ""
	if p.Password != nil {
		pw = *p.Password
	}
	pwEnc, err := s.secret.Seal(pw)
	if err != nil {
		return nil, fmt.Errorf("seal password: %w", err)
	}
	extraEnc, err := s.sealExtra(p.Extra)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var createdBy any
	if p.ActorID != "" {
		createdBy = p.ActorID
	}
	if p.ProjectID == "" {
		return nil, ErrNoProject
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO servers
		(id, project_id, name, name_lower, kind, host, port, options, default_environment,
		 tags, note, enabled, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.ProjectID, p.Name, strings.ToLower(p.Name), string(p.Kind), p.Host, p.Port, optJSON,
		string(p.DefaultEnvironment), model.TagsToString(p.Tags), p.Note,
		boolInt(p.Enabled), createdBy, now, now); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("insert server: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO server_secrets
		(server_id, username, password_enc, extra_enc, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, p.Username, pwEnc, extraEnc, now); err != nil {
		return nil, fmt.Errorf("insert server secret: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return s.GetServer(ctx, id)
}

// UpdateServer는 서버 설정을 교체한다.
//
// 여기서 비밀번호를 한 번 고치면 그 서버의 모든 DB에 반영된다 — 이것이 서버를
// 뽑아낸 이유 그 자체다. 소속 커넥션은 손대지 않는다.
func (s *Store) UpdateServer(ctx context.Context, id string, p SaveServerParams) (*model.Server, error) {
	now := nowString()
	optJSON, err := p.Options.MarshalDB()
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `UPDATE servers SET
		name = ?, name_lower = ?, kind = ?, host = ?, port = ?, options = ?,
		default_environment = ?, tags = ?, note = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		p.Name, strings.ToLower(p.Name), string(p.Kind), p.Host, p.Port, optJSON,
		string(p.DefaultEnvironment), model.TagsToString(p.Tags), p.Note,
		boolInt(p.Enabled), now, id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("update server: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}

	// 비밀번호는 명시적으로 전달된 경우에만 교체한다.
	if p.Password != nil {
		pwEnc, err := s.secret.Seal(*p.Password)
		if err != nil {
			return nil, fmt.Errorf("seal password: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE server_secrets SET username = ?, password_enc = ?, updated_at = ? WHERE server_id = ?`,
			p.Username, pwEnc, now, id); err != nil {
			return nil, fmt.Errorf("update secret: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx,
		`UPDATE server_secrets SET username = ?, updated_at = ? WHERE server_id = ?`,
		p.Username, now, id); err != nil {
		return nil, fmt.Errorf("update secret username: %w", err)
	}
	if p.Extra != nil {
		extraEnc, err := s.sealExtra(p.Extra)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE server_secrets SET extra_enc = ?, updated_at = ? WHERE server_id = ?`,
			extraEnc, now, id); err != nil {
			return nil, fmt.Errorf("update secret extra: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return s.GetServer(ctx, id)
}

func (s *Store) GetServer(ctx context.Context, id string) (*model.Server, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+serverColumns+serverFrom+` WHERE s.id = ?`, id)
	v, err := scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get server: %w", err)
	}
	return v, nil
}

// ListServers는 서버를 반환한다. projectIDs가 nil이면 좁히지 않는다(슈퍼 어드민).
// 빈 슬라이스는 "볼 수 있는 프로젝트가 없다"는 뜻이라 결과도 비어야 한다.
func (s *Store) ListServers(ctx context.Context, projectIDs []string) ([]*model.Server, error) {
	query := `SELECT ` + serverColumns + serverFrom
	args := []any{}
	if projectIDs != nil {
		if len(projectIDs) == 0 {
			// IN () 는 SQLite에서 문법 오류다. 아무것도 맞지 않는 조건으로 바꾼다.
			query += ` WHERE 0`
		} else {
			marks := make([]string, len(projectIDs))
			for i, id := range projectIDs {
				marks[i] = "?"
				args = append(args, id)
			}
			query += ` WHERE s.project_id IN (` + strings.Join(marks, ",") + `)`
		}
	}
	query += ` ORDER BY s.name_lower`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	defer rows.Close()

	out := []*model.Server{}
	for rows.Next() {
		v, err := scanServer(rows)
		if err != nil {
			return nil, fmt.Errorf("scan server: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteServer는 서버와 그 아래 모든 DB를 지운다(CASCADE).
//
// 호출부는 지워질 DB 수를 사용자에게 먼저 보여야 한다. 서버를 지운다는 것은
// 그 서버의 지표·이벤트·스키마 버전·ERD 문서까지 함께 사라진다는 뜻이다.
func (s *Store) DeleteServer(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete server: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetServerSecret은 복호화된 서버 자격증명을 반환한다.
func (s *Store) GetServerSecret(ctx context.Context, serverID string) (*model.Secret, error) {
	var username, pwEnc, extraEnc string
	err := s.db.QueryRowContext(ctx,
		`SELECT username, password_enc, extra_enc FROM server_secrets WHERE server_id = ?`,
		serverID).Scan(&username, &pwEnc, &extraEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return &model.Secret{Extra: map[string]string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get server secret: %w", err)
	}
	return s.openSecret(username, pwEnc, extraEnc)
}

func (s *Store) openSecret(username, pwEnc, extraEnc string) (*model.Secret, error) {
	pw, err := s.secret.Open(pwEnc)
	if err != nil {
		return nil, fmt.Errorf("open password: %w", err)
	}
	extra := map[string]string{}
	if extraEnc != "" {
		raw, err := s.secret.Open(extraEnc)
		if err != nil {
			return nil, fmt.Errorf("open extra: %w", err)
		}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &extra); err != nil {
				return nil, fmt.Errorf("unmarshal extra: %w", err)
			}
		}
	}
	return &model.Secret{Username: username, Password: pw, Extra: extra}, nil
}

// MoveConnectionsToServer는 커넥션들을 다른 서버로 옮긴다(서버 병합).
//
// 이관 마이그레이션은 기존 커넥션을 자동으로 묶지 않는다 — 자격증명이 다를 수 있고
// 봉인된 값은 비교할 수조차 없기 때문이다. 그래서 합치는 일은 값을 확인할 수 있는
// 사람이 여기를 통해 명시적으로 한다. 옮겨진 DB는 **대상 서버의 자격증명을 쓰게 된다.**
func (s *Store) MoveConnectionsToServer(ctx context.Context, targetServerID string, connectionIDs []string) error {
	if len(connectionIDs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	now := nowString()
	for _, id := range connectionIDs {
		res, err := tx.ExecContext(ctx,
			`UPDATE connections SET server_id = ?, updated_at = ? WHERE id = ?`,
			targetServerID, now, id)
		if err != nil {
			if isUniqueViolation(err) {
				// 대상 서버에 같은 이름의 DB가 이미 있다. 조용히 넘기면 옮긴 줄 알았는데
				// 남아 있는 상태가 되므로 전체를 되돌린다.
				return fmt.Errorf("%w: 대상 서버에 같은 DB가 이미 등록되어 있습니다", ErrDuplicateName)
			}
			return fmt.Errorf("move connection: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}
	return tx.Commit()
}

// DeleteEmptyServers는 DB가 하나도 없는 서버를 지운다.
// 병합 뒤에 남는 빈 껍데기를 치우는 용도다.
func (s *Store) DeleteEmptyServers(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM servers WHERE NOT EXISTS (SELECT 1 FROM connections c WHERE c.server_id = servers.id)`)
	if err != nil {
		return 0, fmt.Errorf("delete empty servers: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CreateServerWithDatabase는 서버와 그 첫 DB를 함께 만든다.
//
// 서버를 만들고 DB를 하나도 등록하지 않은 상태는 화면에서 "빈 서버"로만 보이고
// 아무것도 할 수 없다. 등록 흐름이 대개 "이 서버의 이 DB를 보겠다"이므로
// 두 단계를 한 번에 끝낼 수 있어야 한다.
func (s *Store) CreateServerWithDatabase(ctx context.Context, sp SaveServerParams, cp SaveConnectionParams) (*model.Server, *model.Connection, error) {
	srv, err := s.CreateServer(ctx, sp)
	if err != nil {
		return nil, nil, err
	}
	cp.ServerID = srv.ID
	// DB의 프로젝트는 서버의 프로젝트다. 부르는 쪽이 무엇을 넣었든 여기서 맞춘다.
	cp.ProjectID = srv.ProjectID
	if cp.Environment == "" {
		cp.Environment = sp.DefaultEnvironment
	}
	conn, err := s.CreateConnection(ctx, cp)
	if err != nil {
		// 서버만 남기면 사용자는 만들어진 줄 몰랐던 껍데기를 발견하게 된다.
		_ = s.DeleteServer(ctx, srv.ID)
		return nil, nil, err
	}
	srv.DatabaseCount = 1
	return srv, conn, nil
}
