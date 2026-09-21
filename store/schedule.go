package store

import (
	"context"
	"sync"

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
	ss.Mu.Lock()
	defer ss.Mu.Unlock()

	if ss.backend != nil {
		list, err := ss.backend.LoadAll(context.Background())
		if err == nil && len(list) > 0 {
			for _, s := range list {
				ss.Schedules[s.ID] = s
			}
			return
		}
	}
	readJSON(ss.path, &ss.Schedules)
}

func (ss *ScheduleStore) Save() error {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()

	if ss.backend != nil {
		for _, s := range ss.Schedules {
			ss.backend.Save(context.Background(), s)
		}
	} else {
		writeJSON(ss.path, ss.Schedules)
	}
	return nil
}

func (ss *ScheduleStore) List() []*schemas.Schedule {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	var list []*schemas.Schedule
	for _, s := range ss.Schedules {
		list = append(list, s)
	}
	return list
}

func (ss *ScheduleStore) Get(id string) *schemas.Schedule {
	ss.Mu.RLock()
	defer ss.Mu.RUnlock()
	return ss.Schedules[id]
}

func (ss *ScheduleStore) Set(s *schemas.Schedule) {
	ss.Mu.Lock()
	defer ss.Mu.Unlock()
	ss.Schedules[s.ID] = s
	if ss.backend != nil {
		ss.backend.Save(context.Background(), s)
	} else {
		writeJSON(ss.path, ss.Schedules)
	}
}

func (ss *ScheduleStore) GetPath() string {
	return ss.path
}
