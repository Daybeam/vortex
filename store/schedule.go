package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/daybeam/vortex/schemas"
)

type ScheduleStore struct {
	Mu        sync.RWMutex
	path      string
	backend   IScheduleBackend
	Schedules map[string]*schemas.Schedule `json:"schedules"`
}

func NewScheduleStore(path string, backend IScheduleBackend) *ScheduleStore {
	ss := &ScheduleStore{
		path:      path,
		backend:   backend,
		Schedules: make(map[string]*schemas.Schedule),
	}
	ss.Reload()
	return ss
}

func (ss *ScheduleStore) Reload() {
	var loaded map[string]*schemas.Schedule
	if ss.backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		list, err := ss.backend.LoadAll(ctx)
		cancel()
		if err == nil {
			loaded = make(map[string]*schemas.Schedule, len(list))
			for _, s := range list {
				loaded[s.ID] = s
			}
		} else {
			log.Printf("WARN: schedule backend reload failed: %v", err)
			ss.Mu.Lock()
			ss.Schedules = make(map[string]*schemas.Schedule)
			ss.Mu.Unlock()
			return
		}
	}
	if ss.backend != nil {
		ss.Mu.Lock()
		ss.Schedules = loaded
		ss.Mu.Unlock()
		return
	}
	loaded = make(map[string]*schemas.Schedule)
	data, err := os.ReadFile(ss.path)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("WARN: readJSON: failed to read %s: %v", ss.path, err)
		return
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &loaded); err != nil {
			log.Printf("WARN: readJSON: failed to unmarshal %s: %v", ss.path, err)
			return
		}
	}
	ss.Mu.Lock()
	ss.Schedules = loaded
	ss.Mu.Unlock()
}

func (ss *ScheduleStore) Save() error {
	ss.Mu.RLock()
	schedules := make([]*schemas.Schedule, 0, len(ss.Schedules))
	for _, s := range ss.Schedules {
		schedules = append(schedules, s)
	}
	backend := ss.backend
	path := ss.path
	ss.Mu.RUnlock()

	if backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, s := range schedules {
			if err := backend.Save(ctx, s); err != nil {
				return fmt.Errorf("save schedule %s: %w", s.ID, err)
			}
		}
		return nil
	}
	return writeScheduleJSON(path, schedules)
}

func writeScheduleJSON(path string, schedules []*schemas.Schedule) error {
	snapshot := make(map[string]*schemas.Schedule, len(schedules))
	for _, s := range schedules {
		snapshot[s.ID] = s
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func cloneSchedule(s *schemas.Schedule) *schemas.Schedule {
	if s == nil {
		return nil
	}
	cloned := *s
	cloned.TaskInputs = append([]schemas.StepInput(nil), s.TaskInputs...)
	return &cloned
}

func scheduleVisible(s *schemas.Schedule, ownerID string, includeAll bool) bool {
	return s != nil && (includeAll || s.OwnerID != "" && s.OwnerID == ownerID)
}

func (ss *ScheduleStore) List() []*schemas.Schedule {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	var list []*schemas.Schedule
	for _, s := range ss.Schedules {
		list = append(list, cloneSchedule(s))
	}
	return list
}

func (ss *ScheduleStore) ListVisible(ownerID string, includeAll bool) []*schemas.Schedule {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	var list []*schemas.Schedule
	for _, s := range ss.Schedules {
		if scheduleVisible(s, ownerID, includeAll) {
			list = append(list, cloneSchedule(s))
		}
	}
	return list
}

func (ss *ScheduleStore) Get(id string) *schemas.Schedule {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return cloneSchedule(ss.Schedules[id])
}

func (ss *ScheduleStore) GetVisible(id, ownerID string, includeAll bool) *schemas.Schedule {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	s := ss.Schedules[id]
	if !scheduleVisible(s, ownerID, includeAll) {
		return nil
	}
	return cloneSchedule(s)
}

func (ss *ScheduleStore) Set(s *schemas.Schedule) error {
	s = cloneSchedule(s)
	ss.Mu.Lock()
	previous := ss.Schedules[s.ID]
	ss.Schedules[s.ID] = s
	schedules := make([]*schemas.Schedule, 0, len(ss.Schedules))
	for _, scheduled := range ss.Schedules {
		schedules = append(schedules, scheduled)
	}
	backend := ss.backend
	path := ss.path
	ss.Mu.Unlock()
	if backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := backend.Save(ctx, s); err != nil {
			ss.Mu.Lock()
			if previous == nil {
				delete(ss.Schedules, s.ID)
			} else {
				ss.Schedules[s.ID] = previous
			}
			ss.Mu.Unlock()
			return fmt.Errorf("save schedule %s: %w", s.ID, err)
		}
		return nil
	}
	if err := writeScheduleJSON(path, schedules); err != nil {
		ss.Mu.Lock()
		if previous == nil {
			delete(ss.Schedules, s.ID)
		} else {
			ss.Schedules[s.ID] = previous
		}
		ss.Mu.Unlock()
		return err
	}
	return nil
}

func (ss *ScheduleStore) Delete(id string) error {
	ss.Mu.Lock()
	previous := ss.Schedules[id]
	delete(ss.Schedules, id)
	schedules := make([]*schemas.Schedule, 0, len(ss.Schedules))
	for _, scheduled := range ss.Schedules {
		schedules = append(schedules, scheduled)
	}
	backend := ss.backend
	path := ss.path
	ss.Mu.Unlock()
	if backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := backend.Delete(ctx, id); err != nil {
			ss.Mu.Lock()
			if previous != nil {
				ss.Schedules[id] = previous
			}
			ss.Mu.Unlock()
			return fmt.Errorf("delete schedule %s: %w", id, err)
		}
		return nil
	}
	if err := writeScheduleJSON(path, schedules); err != nil {
		ss.Mu.Lock()
		if previous != nil {
			ss.Schedules[id] = previous
		}
		ss.Mu.Unlock()
		return err
	}
	return nil
}

func (ss *ScheduleStore) GetPath() string {
	return ss.path
}
