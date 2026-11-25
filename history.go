package main

import (
	"net/http"
	"slices"
	"strings"
	"time"
)

func deletableRun(run Run) bool {
	switch run.State {
	case success, failed, "cancelled":
		return run.Finished != nil
	case uncertain:
		return run.Finished != nil && run.Reviewed
	default:
		return false
	}
}

func (s *State) deleteRuns(match func(Run) bool) int {
	removed := make(map[string]bool)
	s.Runs = slices.DeleteFunc(s.Runs, func(run Run) bool {
		if deletableRun(run) && match(run) {
			removed[run.ID] = true
			return true
		}
		return false
	})
	for key := range s.Logs {
		id, _, _ := strings.Cut(key, "/")
		if removed[id] {
			delete(s.Logs, key)
		}
	}
	return len(removed)
}

func (a *app) deleteRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := a.store.update(func(s *State) error {
		if s.run(id) == nil {
			return errMissing
		}
		if s.deleteRuns(func(run Run) bool { return run.ID == id }) == 0 {
			return errConflict
		}
		return nil
	})
	if handleWrite(w, err) {
		respond(w, 200, struct{}{})
	}
}

func (a *app) purgeRuns(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days int `json:"days"`
	}
	if !body(w, r, &req) {
		return
	}
	if req.Days < 1 || req.Days > 36500 {
		problem(w, 400, "Bitte ein Alter zwischen 1 und 36.500 Tagen angeben")
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -req.Days)
	deleted := 0
	err := a.store.update(func(s *State) error {
		deleted = s.deleteRuns(func(run Run) bool { return run.Finished.Before(cutoff) })
		return nil
	})
	if handleWrite(w, err) {
		respond(w, 200, map[string]int{"deleted": deleted})
	}
}
