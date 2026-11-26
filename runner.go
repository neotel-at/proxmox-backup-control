package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"
)

type backupAPI interface {
	guests(context.Context) ([]Guest, error)
	storages(context.Context, string) ([]Storage, error)
	start(context.Context, string, int, string) (string, error)
	status(context.Context, string, string) (TaskStatus, error)
	logs(context.Context, string, string, int) ([]LogLine, error)
}

type runner struct {
	store    *store
	clusters map[string]backupAPI
	webhook  string
	mailer   *smtpMailer
	poll     time.Duration
	wake     chan struct{}
}

func (r *runner) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (r *runner) change(fn func(*State) error) {
	if err := r.store.update(fn); err != nil {
		log.Fatalf("Backup-Dienst gestoppt, Zustand konnte nicht sicher gespeichert werden: %v", err)
	}
}
func (r *runner) changeRun(id string, fn func(*Run, *State)) {
	r.change(func(s *State) error { fn(s.run(id), s); return nil })
}
func (r *runner) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if job := nextRun(r.store.snapshot(), time.Now()); job != nil {
			r.execute(ctx, *job)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-ticker.C:
		}
	}
}
func nextRun(state State, now time.Time) *Run {
	var result *Run
	for i := range state.Runs {
		job := &state.Runs[i]
		if job.State == uncertain && !job.Reviewed {
			return nil
		}
		if job.State == running {
			result = job
		}
		if job.State == queued && !job.Due.After(now) && (result == nil || result.State == queued && job.Due.Before(result.Due)) {
			result = job
		}
	}
	return result
}
func (r *runner) execute(ctx context.Context, job Run) {
	event := "Backup-Lauf fortgesetzt"
	if job.State == queued {
		now := time.Now().UTC()
		err := r.store.update(func(saved *State) error {
			run := saved.run(job.ID)
			if run.State != queued {
				return errConflict
			}
			run.State = running
			run.Started = &now
			return nil
		})
		if errors.Is(err, errConflict) {
			return
		}
		if err != nil {
			log.Fatalf("Backup konnte nicht reserviert werden: %v", err)
		}
		event = "Backup-Lauf gestartet"
	}
	log.Printf("%s: lauf=%s plan=%q cluster=%q faellig=%s serie=%t",
		event, job.ID, job.Plan.Name, job.Plan.Cluster, job.Due.UTC().Format(time.RFC3339), job.Recurring)
	api := r.clusters[job.Plan.Cluster]
	final := success
	for i, step := range job.Steps {
		if ctx.Err() != nil {
			return
		}
		result := step.State
		switch result {
		case submitting, uncertain:
			if result == submitting {
				r.finishStep(job.ID, i, uncertain, "Während des Backupstarts wurde der Dienst unterbrochen. Proxmox-Tasks prüfen.")
			}
			result = uncertain
		case pending, running:
			result = r.executeStep(ctx, job, i, api)
			if ctx.Err() != nil {
				return
			}
		}
		switch result {
		case uncertain:
			final = uncertain
		case failed:
			final = failed
		}
		if final == uncertain || final == failed && !job.Plan.Continue {
			break
		}
	}
	now := time.Now().UTC()
	r.changeRun(job.ID, func(saved *Run, _ *State) {
		for i := range saved.Steps {
			if saved.Steps[i].State == pending {
				saved.Steps[i].State = skipped
			}
		}
		saved.State = final
		saved.Finished = &now
		if final != success {
			if r.webhook != "" {
				saved.Notice = &Notice{Next: now, Status: queued}
			}
			if r.mailer != nil && (saved.Plan.MailOnError == nil || *saved.Plan.MailOnError) {
				saved.EmailNotice = &Notice{Next: now, Status: queued}
			}
		}
	})
	log.Printf("Backup-Lauf beendet: lauf=%s plan=%q cluster=%q status=%s",
		job.ID, job.Plan.Name, job.Plan.Cluster, final)
}
func (r *runner) executeStep(ctx context.Context, job Run, index int, api backupAPI) string {
	step := job.Steps[index]
	finish := func(state, message string) string {
		if ctx.Err() != nil {
			return ""
		}
		return r.finishStep(job.ID, index, state, message)
	}
	if api == nil {
		state := failed
		if step.State == running {
			state = uncertain
		}
		return finish(state, "Cluster fehlt in der Konfiguration")
	}
	if step.State == pending {
		guests, err := api.guests(ctx)
		if err != nil {
			return finish(failed, err.Error())
		}
		pos := slices.IndexFunc(guests, func(g Guest) bool { return g.ID == step.Guest.ID && g.Type == step.Guest.Type })
		if pos < 0 || guests[pos].Node == "" {
			return finish(failed, "Gast nicht im Cluster gefunden")
		}
		step.Node = guests[pos].Node
		if job.Plan.TargetNode != "" && job.Plan.TargetNode != step.Node {
			return finish(failed, "Lokales Backup-Ziel ist an "+job.Plan.TargetNode+" gebunden. Der Gast liegt jetzt auf "+step.Node+".")
		}
		targets, err := api.storages(ctx, step.Node)
		if err != nil {
			return finish(failed, err.Error())
		}
		if !slices.ContainsFunc(targets, func(s Storage) bool {
			return s.ID == job.Plan.Storage && s.Type == job.Plan.StorageType && bool(s.Shared) == (job.Plan.TargetNode == "")
		}) {
			return finish(failed, "Backup-Ziel auf "+step.Node+" nicht verfügbar")
		}
		if ctx.Err() != nil {
			return ""
		}
		r.changeRun(job.ID, func(saved *Run, _ *State) {
			saved.Steps[index].State = submitting
			saved.Steps[index].Node = step.Node
		})
		upid, err := api.start(ctx, step.Node, step.Guest.ID, job.Plan.Storage)
		if err != nil {
			var apiErr *pveError
			if errors.Is(err, errBackupNotStarted) || errors.As(err, &apiErr) && slices.Contains([]int{400, 401, 403, 404, 405, 501}, apiErr.code) {
				return finish(failed, err.Error())
			}
			return finish(uncertain, "Backupstart nicht eindeutig bestätigt: "+err.Error())
		}
		step.UPID = upid
		r.changeRun(job.ID, func(saved *Run, _ *State) {
			saved.Steps[index].State = running
			saved.Steps[index].UPID = upid
		})
	}
	var firstError time.Time
	for ctx.Err() == nil {
		status, err := api.status(ctx, step.Node, step.UPID)
		if err == nil && status.Status == "stopped" && status.Exit != "" {
			r.archiveLogs(ctx, api, job.ID, index, step)
			if status.Exit == "OK" {
				return finish(success, "")
			}
			return finish(failed, status.Exit)
		}
		if err == nil && status.Status == "running" {
			firstError = time.Time{}
		} else {
			if firstError.IsZero() {
				firstError = time.Now()
			}
			if time.Since(firstError) >= 5*time.Minute {
				return finish(uncertain, fmt.Sprintf("Taskstatus fünf Minuten nicht abrufbar: %s %s %v", status.Status, status.Exit, err))
			}
		}
		if !pause(ctx, r.poll) {
			return ""
		}
	}
	return ""
}
func (r *runner) finishStep(id string, index int, state, message string) string {
	r.changeRun(id, func(saved *Run, _ *State) {
		saved.Steps[index].State = state
		saved.Steps[index].Error = message
	})
	return state
}

const maxLogLines = 10000

func (r *runner) archiveLogs(ctx context.Context, api backupAPI, id string, index int, step Step) {
	var all []LogLine
	var warning string
	for len(all) < maxLogLines && ctx.Err() == nil {
		lines, err := api.logs(ctx, step.Node, step.UPID, len(all))
		if err != nil {
			warning = err.Error()
			break
		}
		if len(lines) == 0 {
			break
		}
		if len(all) > 0 && lines[0].Number <= all[len(all)-1].Number {
			warning = "Proxmox lieferte wiederholte Logzeilen"
			break
		}
		all = append(all, lines...)
	}
	if len(all) >= maxLogLines {
		all = all[:maxLogLines]
		warning = "Lokales Archiv auf 10.000 Zeilen begrenzt"
	}
	if ctx.Err() != nil {
		return
	}
	r.changeRun(id, func(saved *Run, s *State) {
		s.Logs[logKey(id, index)] = all
		saved.Steps[index].LogError = warning
	})
}
func pause(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type notificationChannel struct {
	name   string
	notice func(*Run) *Notice
	send   func(context.Context, Run) error
}

func (r *runner) notifyLoop(ctx context.Context) {
	var workers sync.WaitGroup
	start := func(channel notificationChannel) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			r.pollNotices(ctx, channel)
		}()
	}
	if r.mailer != nil {
		start(notificationChannel{
			name: "E-Mail", notice: func(run *Run) *Notice { return run.EmailNotice }, send: r.mailer.send,
		})
	}
	if r.webhook != "" {
		client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: noRedirect}
		start(notificationChannel{
			name: "Webhook", notice: func(run *Run) *Notice { return run.Notice },
			send: func(ctx context.Context, run Run) error { return sendNotice(ctx, client, r.webhook, run) },
		})
	}
	workers.Wait()
}

func (r *runner) pollNotices(ctx context.Context, channel notificationChannel) {
	for ctx.Err() == nil {
		for _, job := range r.store.snapshot().Runs {
			if ctx.Err() != nil {
				return
			}
			r.deliverNotice(ctx, job, channel)
		}
		if !pause(ctx, 5*time.Second) {
			return
		}
	}
}

func (r *runner) deliverNotice(ctx context.Context, job Run, channel notificationChannel) {
	notice := channel.notice(&job)
	if notice == nil || notice.Status != queued || notice.Next.After(time.Now()) {
		return
	}
	err := channel.send(ctx, job)
	if err != nil && ctx.Err() != nil {
		return
	}
	if err != nil {
		log.Printf("%s für Lauf %s: %v", channel.name, job.ID, err)
	}
	r.changeRun(job.ID, func(saved *Run, _ *State) {
		if saved == nil {
			return // History entry was deleted during delivery.
		}
		notice := channel.notice(saved)
		notice.Attempts++
		switch {
		case err == nil:
			notice.Status = success
		case notice.Attempts >= 5:
			notice.Status = failed
		default:
			notice.Next = time.Now().UTC().Add(time.Minute * time.Duration(1<<(notice.Attempts-1)))
		}
	})
	if err == nil {
		log.Printf("%s für Lauf %s: Versand bestätigt (Versuch %d)", channel.name, job.ID, notice.Attempts+1)
	}
}

func sendNotice(ctx context.Context, client *http.Client, endpoint string, job Run) error {
	body, err := json.Marshal(struct {
		Event string `json:"event"`
		Run   Run    `json:"run"`
	}{"pve_backup_attention", job})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("ungültige Webhook-Anfrage")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", job.ID)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("Webhook nicht erreichbar")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
