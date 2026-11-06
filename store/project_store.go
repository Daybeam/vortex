package store

import (
	"context"
	"database/sql"
	"time"
)

// Project represents a project space that groups related tasks.
// Tasks in the same project share ScopeProject memory items.
type Project struct {
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	RootPath  string    `json:"root_path,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProjectStore provides CRUD operations for project spaces.
type ProjectStore struct {
	db *sql.DB
}

// NewProjectStore creates a ProjectStore backed by the given DB.
func NewProjectStore(db *sql.DB) *ProjectStore {
	return &ProjectStore{db: db}
}

// CreateProject inserts a new project. Returns error if the project_id already exists.
func (s *ProjectStore) CreateProject(ctx context.Context, p *Project) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (project_id, name, root_path) VALUES (?, ?, ?)`,
		p.ProjectID, p.Name, p.RootPath)
	return err
}

// GetProject fetches a project by ID. Returns sql.ErrNoRows if not found.
func (s *ProjectStore) GetProject(ctx context.Context, projectID string) (*Project, error) {
	var p Project
	err := s.db.QueryRowContext(ctx,
		`SELECT project_id, name, COALESCE(root_path, ''), created_at, updated_at FROM projects WHERE project_id = ?`,
		projectID).Scan(&p.ProjectID, &p.Name, &p.RootPath, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListProjects returns all projects ordered by most recently updated.
func (s *ProjectStore) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id, name, COALESCE(root_path, ''), created_at, updated_at FROM projects ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []*Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ProjectID, &p.Name, &p.RootPath, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		projects = append(projects, &p)
	}
	return projects, rows.Err()
}

// UpdateProject updates a project's name and root_path.
func (s *ProjectStore) UpdateProject(ctx context.Context, projectID, name, rootPath string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE projects SET name = ?, root_path = ?, updated_at = CURRENT_TIMESTAMP WHERE project_id = ?`,
		name, rootPath, projectID)
	return err
}

// DeleteProject removes a project. Tasks referencing this project_id are NOT
// deleted — their project_id remains as a dangling reference (reverts to
// "default" behavior in filtering since no project record exists).
func (s *ProjectStore) DeleteProject(ctx context.Context, projectID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE project_id = ?`, projectID)
	return err
}

// ListTaskIDsByProject returns task IDs belonging to the given project.
func (s *ProjectStore) ListTaskIDsByProject(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT task_id FROM tasks WHERE project_id = ? ORDER BY updated_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var taskIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		taskIDs = append(taskIDs, id)
	}
	return taskIDs, rows.Err()
}

// SetTaskProject associates a task with a project. If the project doesn't
// exist in the projects table, the task still gets the project_id — the
// projects table is metadata-only, not a foreign key constraint.
func (s *ProjectStore) SetTaskProject(ctx context.Context, taskID, projectID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET project_id = ?, updated_at = CURRENT_TIMESTAMP WHERE task_id = ?`,
		projectID, taskID)
	return err
}
