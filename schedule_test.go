package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testTime(t *testing.T, value string) time.Time {
	t.Helper()
	point, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return point
}

func TestNextScheduleAcrossClockChanges(t *testing.T) {
	for _, tc := range []struct {
		name        string
		days        []time.Weekday
		after, want string
	}{
		{"weekly", []time.Weekday{time.Monday, time.Wednesday, time.Friday}, "2026-11-23T01:30:00Z", "2026-11-25T01:30:00Z"},
		{"spring skips nonexistent hour", []time.Weekday{time.Sunday}, "2026-03-28T12:00:00Z", "2026-04-05T00:30:00Z"},
		{"autumn picks first occurrence", []time.Weekday{time.Sunday}, "2026-10-24T12:00:00Z", "2026-10-25T00:30:00Z"},
		{"autumn does not run again", []time.Weekday{time.Sunday}, "2026-10-25T00:30:01Z", "2026-11-01T01:30:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schedule := Schedule{scheduleInput: scheduleInput{Clock: "02:30", Days: tc.days}, Zone: defaultScheduleZone}
			next, err := schedule.nextAfter(testTime(t, tc.after))
			if err != nil || !next.Equal(testTime(t, tc.want)) {
				t.Fatalf("next = %v, %v; want %s", next, err, tc.want)
			}
		})
	}
}

func TestRecurringSchedulePersistsAndCoalesces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"plans":[{"id":"p","name":"Original","cluster":"demo","storage":"pbs","guests":[{"vmid":101}]}],"runs":[{"id":"old","plan":{"id":"p"},"state":"queued","due":"2030-01-01T00:00:00Z","steps":[]}],"logs":{"old/0":[{"n":1,"t":"existing log"}]}}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.lock.Close() }()
	worker := &runner{store: s}
	a := &app{runner: worker, scheduleZone: defaultScheduleZone}
	cookie := testLogin(t, a)
	post := func(route, payload string, code int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, route, strings.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-PVE-Request", "1")
		request.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.handler().ServeHTTP(w, request)
		if w.Code != code {
			t.Fatalf("HTTP %d, want %d: %s", w.Code, code, w.Body.String())
		}
		return w
	}
	route := "/api/plans/p/schedule"
	post(route, `{"clock":"25:00","days":[1]}`, 400)
	post(route, `{"clock":"03:00","days":[1,1]}`, 400)
	post(route, `{"clock":"03:00","days":[]}`, 400)
	payload := `{"clock":"03:00","days":[6,5,4,3,2,1,0]}`
	w := post(route, payload, 200)
	var saved Schedule
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || saved.Next.IsZero() || saved.Zone != defaultScheduleZone {
		t.Fatalf("saved schedule = %+v, %v", saved, err)
	}
	if err := s.lock.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	worker.store = s
	reopenedState := s.snapshot()
	if !reopenedState.Schedules["p"].Next.Equal(saved.Next) || reopenedState.Logs["old/0"][0].Text != "existing log" {
		t.Fatal("schedule and existing log did not survive restart")
	}
	// The worker missed three daily starts while offline.
	now := saved.Next.Add(72 * time.Hour)
	for _, point := range []time.Time{now, now, now.Add(24 * time.Hour)} {
		if err := worker.enqueueScheduled(point); err != nil {
			t.Fatal(err)
		}
	}
	current := s.snapshot()
	if len(current.Runs) != 2 || current.Runs[0].ID != "old" || !current.Runs[1].Recurring ||
		!current.Runs[1].Due.Equal(saved.Next) || current.Runs[1].Steps[0].Guest.ID != 101 ||
		!current.Schedules["p"].Next.After(now.Add(24*time.Hour)) {
		t.Fatalf("unexpected catch-up state: %+v", current)
	}
	if err := s.update(func(next *State) error {
		next.Runs[1].State = success
		next.Plans[0].Name = "Edited"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := worker.enqueueScheduled(current.Schedules["p"].Next); err != nil {
		t.Fatal(err)
	}
	current = s.snapshot()
	if len(current.Runs) != 3 || current.Runs[2].Plan.Name != "Edited" || current.Runs[1].Plan.Name != "Original" {
		t.Fatal("a new series run must use the current plan without rewriting history")
	}
	post(route, "null", 200)
	if err := worker.enqueueScheduled(current.Schedules["p"].Next); err != nil {
		t.Fatal(err)
	}
	current = s.snapshot()
	if len(current.Schedules) != 0 || len(current.Runs) != 3 {
		t.Fatal("removing the schedule must preserve existing runs")
	}
}
