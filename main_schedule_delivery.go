package main

import (
	"database/sql"
	"net/http"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/pkg/api"
	"github.com/daybeam/vortex/store"
)

func wireScheduleDelivery(sm *core.ScheduleManager, db *sql.DB) {
	if db != nil {
		sm.SetDeliveryOutbox(store.NewSQLiteChatOutbox(db))
	}
}

func registerScheduleRoutes(mux *http.ServeMux, h *api.APIHandler) {
	mux.HandleFunc("/api/schedules/inbox", h.ScheduleInboxHandler)
	mux.HandleFunc("/api/schedules", h.SchedulesHandler)
	mux.HandleFunc("/api/schedules/create", h.ScheduleCreateHandler)
	mux.HandleFunc("/api/schedules/pause", h.SchedulePauseHandler)
	mux.HandleFunc("/api/schedules/delete", h.ScheduleDeleteHandler)
	mux.HandleFunc("/api/schedules/update", h.ScheduleUpdateHandler)
	mux.HandleFunc("/api/schedules/run", h.ScheduleRunNowHandler)
}
