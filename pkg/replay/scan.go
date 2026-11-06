package replay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is returned when a task has neither telemetry nor artifacts.
var ErrNotFound = errors.New("replay: run not found")

var taskIDPattern = regexp.MustCompile(`^task_[A-Za-z0-9_-]{1,80}$`)

// Loader reads historical runs from the log and task directories.
type Loader struct {
	logDir   string
	tasksDir string
	// now is the clock used to decide whether a run is still in flight. It is
	// a field so tests can pin it; production always uses time.Now.
	now func() time.Time
}

// NewLoader builds a loader rooted at the telemetry log directory and the
// task artifact directory.
func NewLoader(logDir, tasksDir string) *Loader {
	return &Loader{logDir: logDir, tasksDir: tasksDir, now: time.Now}
}

type logFile struct {
	date string
	path string
}

type rawEvent struct {
	Event      string         `json:"event"`
	TaskID     string         `json:"task_id"`
	StepID     string         `json:"step_id"`
	Detail     map[string]any `json:"detail"`
	Confidence *float64       `json:"confidence"`
	TS         float64        `json:"ts"`
	date       string
}

// Index lists replayable runs newest-first plus task directories that only
// carry stored step results.
func (l *Loader) Index(days, limit int) (Index, error) {
	files, err := l.taskLogFiles(days)
	if err != nil {
		return Index{}, err
	}
	index := Index{Runs: []RunSummary{}}
	seen := make(map[string]bool, len(files))
	for taskID, taskFiles := range files {
		detail, err := buildRun(taskID, taskFiles, l.now())
		if err != nil {
			continue
		}
		detail.Artifacts = l.artifactNames(taskID)
		detail.RunSummary.Artifacts = len(detail.Artifacts)
		index.Runs = append(index.Runs, detail.RunSummary)
		seen[taskID] = true
	}
	sort.Slice(index.Runs, func(i, j int) bool {
		if index.Runs[i].StartedAt != index.Runs[j].StartedAt {
			return index.Runs[i].StartedAt > index.Runs[j].StartedAt
		}
		return index.Runs[i].TaskID < index.Runs[j].TaskID
	})
	if limit > 0 && len(index.Runs) > limit {
		index.Runs = index.Runs[:limit]
	}
	index.ArtifactOnly = l.artifactOnly(seen)
	return index, nil
}

// Detail reconstructs one run. Tasks with stored artifacts but no telemetry
// return a detail marked as not replayable instead of an error.
func (l *Loader) Detail(taskID string) (*RunDetail, error) {
	if !taskIDPattern.MatchString(taskID) {
		return nil, ErrNotFound
	}
	files, err := l.taskLogFiles(0)
	if err != nil {
		return nil, err
	}
	if taskFiles, ok := files[taskID]; ok && len(taskFiles) > 0 {
		detail, err := buildRun(taskID, taskFiles, l.now())
		if err != nil {
			return nil, err
		}
		detail.Artifacts = l.artifactNames(taskID)
		detail.RunSummary.Artifacts = len(detail.Artifacts)
		applyGraph(detail, l.graph(taskID))
		return detail, nil
	}
	artifacts := l.artifactNames(taskID)
	if len(artifacts) == 0 {
		return nil, ErrNotFound
	}
	return &RunDetail{
		RunSummary: RunSummary{
			TaskID:     taskID,
			Title:      taskID,
			Status:     StatusArtifacts,
			Replayable: false,
			Artifacts:  len(artifacts),
		},
		Steps:     []Step{},
		Events:    []Event{},
		Artifacts: artifacts,
	}, nil
}

// taskLogFiles groups task_*.jsonl files by task id across the newest date
// directories. days <= 0 scans every date directory.
func (l *Loader) taskLogFiles(days int) (map[string][]logFile, error) {
	out := map[string][]logFile{}
	entries, err := os.ReadDir(l.logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	var dates []string
	for _, entry := range entries {
		if entry.IsDir() && isDateDir(entry.Name()) {
			dates = append(dates, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	if days > 0 && len(dates) > days {
		dates = dates[:days]
	}
	for _, date := range dates {
		dir := filepath.Join(l.logDir, date)
		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range dirEntries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasPrefix(name, "task_") || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			taskID := strings.TrimSuffix(name, ".jsonl")
			if !taskIDPattern.MatchString(taskID) {
				continue
			}
			out[taskID] = append(out[taskID], logFile{date: date, path: filepath.Join(dir, name)})
		}
	}
	for taskID := range out {
		sort.Slice(out[taskID], func(i, j int) bool { return out[taskID][i].date < out[taskID][j].date })
	}
	return out, nil
}

func isDateDir(name string) bool {
	if len(name) != len("2006-01-02") {
		return false
	}
	_, err := time.Parse("2006-01-02", name)
	return err == nil
}

func parseFile(file logFile) []rawEvent {
	handle, err := os.Open(file.path)
	if err != nil {
		return nil
	}
	defer handle.Close()
	var out []rawEvent
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event rawEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		if event.Event == "" || event.TS <= 0 {
			continue
		}
		event.date = file.date
		out = append(out, event)
	}
	return out
}

func (l *Loader) artifactNames(taskID string) []string {
	if l.tasksDir == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(l.tasksDir, taskID))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func (l *Loader) artifactOnly(exclude map[string]bool) []ArtifactOnly {
	if l.tasksDir == "" {
		return nil
	}
	entries, err := os.ReadDir(l.tasksDir)
	if err != nil {
		return nil
	}
	var out []ArtifactOnly
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, "task_") || exclude[name] {
			continue
		}
		names := l.artifactNames(name)
		if len(names) == 0 {
			continue
		}
		updated := ""
		if info, err := entry.Info(); err == nil {
			updated = info.ModTime().UTC().Format(time.RFC3339)
		}
		out = append(out, ArtifactOnly{TaskID: name, Artifacts: len(names), UpdatedAt: updated})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

type graphFile struct {
	Goal  string `json:"goal"`
	Steps []struct {
		ID        string   `json:"id"`
		RoleID    string   `json:"role_id"`
		Task      string   `json:"task"`
		DependsOn []string `json:"depends_on"`
	} `json:"steps"`
}

func (l *Loader) graph(taskID string) *Graph {
	if l.tasksDir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(l.tasksDir, taskID+".json"))
	if err != nil {
		return nil
	}
	var file graphFile
	if err := json.Unmarshal(data, &file); err != nil || len(file.Steps) == 0 {
		return nil
	}
	graph := &Graph{Goal: file.Goal, Steps: make([]GraphStep, 0, len(file.Steps))}
	for _, step := range file.Steps {
		graph.Steps = append(graph.Steps, GraphStep{
			ID:        step.ID,
			Role:      step.RoleID,
			Task:      step.Task,
			DependsOn: step.DependsOn,
		})
	}
	return graph
}

// applyGraph enriches steps with explicit dependency, role and task metadata
// when the run came from a stored task-graph definition.
func applyGraph(detail *RunDetail, graph *Graph) {
	if graph == nil {
		return
	}
	detail.Graph = graph
	byID := make(map[string]GraphStep, len(graph.Steps))
	for _, step := range graph.Steps {
		byID[step.ID] = step
	}
	for i := range detail.Steps {
		step, ok := byID[detail.Steps[i].ID]
		if !ok {
			continue
		}
		detail.Steps[i].DependsOn = step.DependsOn
		if detail.Steps[i].Role == "" {
			detail.Steps[i].Role = step.Role
		}
		if detail.Steps[i].Task == "" {
			detail.Steps[i].Task = oneLine(step.Task, 400)
		}
	}
}
