package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProxmoxSequentialBackupAndArchivedLogs(t *testing.T) {
	var mu sync.Mutex
	var events []string
	statusCalls := map[string]int{}
	pve := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "PVEAPIToken=test@pve!gui=fixture" {
			t.Error("incorrect Proxmox authorization")
		}
		reply := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api2/json/"), "/")
		switch {
		case len(parts) == 2 && parts[0] == "cluster":
			reply([]map[string]any{
				{"vmid": 102, "node": "pve-2", "type": "lxc"},
				{"vmid": 101, "node": "pve-1", "type": "qemu"},
				{"vmid": 900, "node": "pve-1", "type": "qemu", "template": 1},
			})
		case len(parts) == 3 && parts[2] == "storage":
			reply([]map[string]any{{"storage": "pbs", "type": "pbs", "content": "backup", "active": 1, "enabled": true, "shared": 1}})
		case len(parts) == 3 && parts[2] == "vzdump":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Method != "POST" || r.Form.Get("remove") != "0" || r.Form.Get("mode") != "snapshot" || r.Form.Get("storage") != "pbs" ||
				r.Form.Get("notes-template") != "{{guestname}}" || r.Form.Get("notification-mode") != "none" {
				t.Errorf("unexpected backup request: %s %v", r.Method, r.Form)
			}
			id := r.Form.Get("vmid")
			events = append(events, "start "+id+" "+parts[1])
			reply(fmt.Sprintf("UPID:%s:00000001:00000001:00000001:vzdump:%s:test@pve!gui:", parts[1], id))
		case len(parts) == 5 && parts[4] == "status":
			id := strings.Split(parts[3], ":")[6]
			statusCalls[id]++
			if statusCalls[id] == 1 {
				reply(TaskStatus{Status: "running"})
				return
			}
			events = append(events, "stop "+id)
			exit := "OK"
			if id == "102" {
				exit = "backup failed"
			}
			reply(TaskStatus{Status: "stopped", Exit: exit})
		case len(parts) == 5 && parts[4] == "log":
			if r.URL.Query().Get("start") == "0" {
				reply([]LogLine{{1, "backup output"}})
			} else {
				reply([]LogLine{})
			}
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer pve.Close()
	client := &pveClient{pve.URL + "/api2/json", "PVEAPIToken=test@pve!gui=fixture", pve.Client()}
	guests, err := client.guests(context.Background())
	if err != nil || len(guests) != 2 || guests[0].ID != 101 {
		t.Fatalf("inventory = %v, %v", guests, err)
	}
	// Update the current node from inventory instead of the node stored in the plan.
	guests[0].Node = "previous-node"
	worker := testRunner(t, client)
	worker.mailer = &smtpMailer{}
	job := Run{ID: "run", State: queued, Plan: Plan{Cluster: "test", Storage: "pbs", StorageType: "pbs", Continue: true},
		Steps: []Step{{Guest: guests[1], State: pending}, {Guest: guests[0], State: pending}}}
	worker.change(func(s *State) error { s.Runs = append(s.Runs, job); return nil })
	worker.execute(context.Background(), job)

	mu.Lock()
	if want := []string{"start 102 pve-2", "stop 102", "start 101 pve-1", "stop 101"}; !slices.Equal(events, want) {
		t.Errorf("events %v, want %v", events, want)
	}
	mu.Unlock()

	state := worker.store.snapshot()
	got := state.run("run")
	if got.State != failed || got.Steps[0].State != failed || got.Steps[1].State != success || got.Notice == nil || got.EmailNotice == nil || got.EmailNotice.Status != queued {
		t.Fatalf("unexpected run: %+v", got)
	}
	if lines := state.Logs["run/1"]; len(lines) != 1 || lines[0].Text != "backup output" {
		t.Fatalf("archived log: %v", lines)
	}
	if err := worker.store.lock.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(worker.store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.lock.Close()
	if state := reopened.snapshot(); state.run("run").State != failed || len(state.Logs["run/1"]) != 1 {
		t.Fatal("result and log did not survive reopening")
	}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event struct {
			Event string
			Run   Run
		}
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil || event.Event != "pve_backup_attention" || event.Run.ID != "run" || r.Header.Get("Idempotency-Key") != "run" {
			t.Errorf("unexpected notification: %+v, %v", event, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	if err := sendNotice(context.Background(), hook.Client(), hook.URL, *got); err != nil {
		t.Fatal(err)
	}
}

func TestBackupNotificationCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name  string
		modes []string
		other bool
	}{
		{name: "modern", modes: []string{"none"}},
		{name: "legacy", modes: []string{"none", "legacy-sendmail"}},
		{name: "no notification-mode option", modes: []string{"none", "legacy-sendmail", ""}},
		{name: "unrelated validation failure", modes: []string{"none"}, other: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var modes []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if err := req.ParseForm(); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				modes = append(modes, req.Form.Get("notification-mode"))
				if len(modes) > 1 && (!req.Form.Has("mailto") || req.Form.Get("mailto") != "") {
					t.Error("fallback must override configured SMTP recipients")
				}
				if tc.other || len(modes) < len(tc.modes) {
					field := "notification-mode"
					if tc.other {
						field = "storage"
					}
					w.WriteHeader(400)
					_ = json.NewEncoder(w).Encode(map[string]any{"errors": map[string]string{field: "parameter rejected"}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"data": "UPID:pve:00000001:00000001:00000001:vzdump:101:test@pve:"})
			}))
			defer server.Close()
			client := &pveClient{server.URL, "fixture", server.Client()}
			_, err := client.start(context.Background(), "pve", 101, "pbs")
			if (err != nil) != tc.other {
				t.Errorf("start error: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(modes, tc.modes) {
				t.Errorf("notification modes: got %v, want %v", modes, tc.modes)
			}
		})
	}
}

func TestPerPlanFailureEmail(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name  string
		state string
		mail  *bool
		want  bool
	}{
		{name: "existing plans retain failure mail", state: failed, want: true},
		{name: "enabled on failure", state: failed, mail: &on, want: true},
		{name: "disabled on failure", state: failed, mail: &off},
		{name: "enabled on success", state: success, mail: &on},
		{name: "enabled when outcome needs review", state: uncertain, mail: &on, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := testRunner(t, nil)
			worker.mailer = &smtpMailer{}
			job := Run{ID: "run", State: running, Plan: Plan{MailOnError: tc.mail}, Steps: []Step{{State: tc.state}}}
			worker.change(func(s *State) error { s.Runs = append(s.Runs, job); return nil })
			worker.execute(context.Background(), job)
			state := worker.store.snapshot()
			finished := state.run(job.ID)
			if (finished.EmailNotice != nil) != tc.want {
				t.Errorf("mail queued: got %v, want %v", finished.EmailNotice != nil, tc.want)
			}
			if (finished.Notice != nil) != (tc.state != success) {
				t.Errorf("webhook changed for state %s", tc.state)
			}
		})
	}
}

func testRunner(t *testing.T, api backupAPI) *runner {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.lock.Close() })
	return &runner{store: s, clusters: map[string]backupAPI{"test": api}, webhook: "https://webhook.invalid", poll: time.Millisecond}
}

type recoveringAPI struct {
	backupAPI
	starts int
}

func (api *recoveringAPI) start(context.Context, string, int, string) (string, error) {
	api.starts++
	return "", errors.New("unexpected duplicate start")
}
func (*recoveringAPI) status(context.Context, string, string) (TaskStatus, error) {
	return TaskStatus{Status: "stopped", Exit: "OK"}, nil
}
func (*recoveringAPI) logs(context.Context, string, string, int) ([]LogLine, error) { return nil, nil }

func TestRestartStatesAndQueueHold(t *testing.T) {
	api := &recoveringAPI{}
	worker := testRunner(t, api)
	for _, tc := range []struct{ phase, upid, want string }{
		{running, "known", success},
		{submitting, "", uncertain},
	} {
		job := Run{ID: tc.phase, State: running, Plan: Plan{Cluster: "test"}, Steps: []Step{{State: tc.phase, Node: "pve-1", UPID: tc.upid}}}
		worker.change(func(s *State) error { s.Runs = append(s.Runs, job); return nil })
		worker.execute(context.Background(), job)
		state := worker.store.snapshot()
		if got := state.run(job.ID).State; got != tc.want {
			t.Fatalf("state after restart = %s; want %s", got, tc.want)
		}
	}
	if api.starts != 0 {
		t.Fatalf("restart started %d duplicate backups", api.starts)
	}
	now := time.Now()
	worker.change(func(s *State) error {
		s.Runs = append(s.Runs, Run{ID: "future", State: queued, Due: now.Add(time.Hour)}, Run{ID: "next", State: queued, Due: now})
		return nil
	})
	if nextRun(worker.store.snapshot(), now) != nil {
		t.Fatal("uncertain run did not stop the queue")
	}
	worker.changeRun(submitting, func(job *Run, _ *State) { job.Reviewed = true })
	if next := nextRun(worker.store.snapshot(), now); next == nil || next.ID != "next" {
		t.Fatalf("unexpected next run: %v", next)
	}
}

func TestVZDumpNoOpAndCSRF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("unexpected method: %s", r.Method)
		}
		_, _ = fmt.Fprint(w, `{"data":"OK"}`)
	}))
	defer server.Close()
	pve := &pveClient{server.URL, "fixture", server.Client()}
	if upid, err := pve.start(context.Background(), "pve-1", 101, "pbs"); upid != "" || !errors.Is(err, errBackupNotStarted) {
		t.Fatalf("no-op = %s, %v", upid, err)
	}
	a := &app{runner: testRunner(t, pve)}
	cookie := testLogin(t, a)
	for _, tc := range []struct {
		header bool
		status int
	}{{false, 403}, {true, 404}} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/plans/"+strconv.Itoa(tc.status)+"/runs", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		if tc.header {
			req.Header.Set("X-PVE-Request", "1")
		}
		req.AddCookie(cookie)
		a.handler().ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("status %d, want %d", w.Code, tc.status)
		}
	}
}
