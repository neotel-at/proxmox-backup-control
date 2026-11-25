package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"time"
	_ "time/tzdata"
)

const defaultScheduleZone = "Europe/Vienna"

type scheduleInput struct {
	Clock string         `json:"clock"`
	Days  []time.Weekday `json:"days"`
}

type Schedule struct {
	scheduleInput
	Zone string    `json:"zone"`
	Next time.Time `json:"next"`
}

func scheduleLocation(zone string) (*time.Location, error) {
	if zone == "" || zone == "Local" {
		return nil, errors.New("eine feste IANA-Zeitzone ist erforderlich")
	}
	return time.LoadLocation(zone)
}

func (input scheduleInput) slots() (time.Time, error) {
	clock, err := time.Parse("15:04", input.Clock)
	if err != nil || clock.Format("15:04") != input.Clock {
		return time.Time{}, errors.New("Uhrzeit im Format HH:MM erforderlich")
	}
	if len(input.Days) == 0 || len(input.Days) > 7 {
		return time.Time{}, errors.New("mindestens einen Wochentag auswählen")
	}
	var seen [7]bool
	for _, day := range input.Days {
		if day < time.Sunday || day > time.Saturday || seen[day] {
			return time.Time{}, errors.New("ungültige oder doppelte Wochentage")
		}
		seen[day] = true
	}
	return clock, nil
}

func (schedule Schedule) nextAfter(after time.Time) (time.Time, error) {
	clock, err := schedule.slots()
	if err != nil {
		return time.Time{}, err
	}
	location, err := scheduleLocation(schedule.Zone)
	if err != nil {
		return time.Time{}, err
	}
	year, month, day := after.In(location).Date()
	// Use UTC for calendar arithmetic so DST cannot normalize a local date.
	date := time.Date(year, month, day, clock.Hour(), clock.Minute(), 0, 0, time.UTC)
	for offset := 0; offset <= 14; offset++ {
		wall := date.AddDate(0, 0, offset)
		if !slices.Contains(schedule.Days, wall.Weekday()) {
			continue
		}
		occurrence := firstLocalOccurrence(wall, location)
		if occurrence.After(after) {
			return occurrence, nil
		}
	}
	return time.Time{}, errors.New("kein nächster Termin gefunden")
}

func firstLocalOccurrence(wall time.Time, location *time.Location) time.Time {
	// Collect the UTC offsets on either side of a possible clock change.
	// A skipped local time has no match and a repeated time uses its first match.
	end := wall.Add(24 * time.Hour)
	var first time.Time
	wallYear, wallMonth, wallDay := wall.Date()
	for probe := wall.Add(-24 * time.Hour); !probe.After(end); {
		local := probe.In(location)
		_, offset := local.Zone()
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		actual := candidate.In(location)
		year, month, day := actual.Date()
		if year == wallYear && month == wallMonth && day == wallDay &&
			actual.Hour() == wall.Hour() && actual.Minute() == wall.Minute() && actual.Second() == 0 &&
			(first.IsZero() || candidate.Before(first)) {
			first = candidate
		}
		_, boundary := local.ZoneBounds()
		if boundary.IsZero() || !boundary.After(probe) {
			break
		}
		probe = boundary
	}
	return first
}

func (a *app) setSchedule(w http.ResponseWriter, r *http.Request) {
	var input *scheduleInput
	if !body(w, r, &input) {
		return
	}
	if input != nil {
		if _, err := input.slots(); err != nil {
			problem(w, 400, err.Error())
			return
		}
		slices.Sort(input.Days)
	}
	id := r.PathValue("id")
	var result *Schedule
	err := a.store.update(func(s *State) error {
		if !slices.ContainsFunc(s.Plans, func(plan Plan) bool { return plan.ID == id }) {
			return errMissing
		}
		if input == nil {
			delete(s.Schedules, id)
			return nil
		}
		schedule := Schedule{scheduleInput: *input, Zone: a.scheduleZone}
		if saved, exists := s.Schedules[id]; exists {
			schedule.Zone = saved.Zone
			// A retry of the same rule must preserve a pending occurrence.
			if saved.Clock == input.Clock && slices.Equal(saved.Days, input.Days) {
				result = &saved
				return nil
			}
		}
		next, err := schedule.nextAfter(time.Now())
		if err != nil {
			return err
		}
		schedule.Next = next
		if s.Schedules == nil {
			s.Schedules = make(map[string]Schedule)
		}
		s.Schedules[id] = schedule
		result = &schedule
		return nil
	})
	if handleWrite(w, err) {
		respond(w, 200, result)
	}
}

func newRun(plan Plan, due time.Time, recurring bool) Run {
	job := Run{ID: newID(), Plan: plan, Due: due, State: queued, Recurring: recurring}
	for _, guest := range plan.Guests {
		job.Steps = append(job.Steps, Step{Guest: guest, State: pending})
	}
	return job
}

func (r *runner) scheduleLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := r.enqueueScheduled(time.Now()); err != nil {
			log.Printf("Wiederkehrende Backuptermine: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *runner) enqueueScheduled(now time.Time) error {
	due := false
	for _, schedule := range r.store.snapshot().Schedules {
		if !schedule.Next.After(now) {
			due = true
			break
		}
	}
	if !due {
		return nil
	}
	enqueued := false
	err := r.store.update(func(s *State) error {
		waiting := make(map[string]bool)
		for _, run := range s.Runs {
			if run.Recurring && run.State == queued {
				waiting[run.Plan.ID] = true
			}
		}
		for _, plan := range s.Plans {
			schedule, ok := s.Schedules[plan.ID]
			if !ok || schedule.Next.After(now) {
				continue
			}
			next, err := schedule.nextAfter(now)
			if err != nil {
				return fmt.Errorf("Plan %s: %w", plan.Name, err)
			}
			if !waiting[plan.ID] {
				s.Runs = append(s.Runs, newRun(plan, schedule.Next, true))
				enqueued = true
			}
			schedule.Next = next
			s.Schedules[plan.ID] = schedule
		}
		return nil
	})
	if err == nil && enqueued {
		r.signal()
	}
	return err
}
