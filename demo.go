package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type demoAPI struct {
	id, name  string
	inventory []Guest
	mu        sync.Mutex
	tasks     map[string]demoTask
}
type demoTask struct {
	started time.Time
	fail    bool
	guest   int
	node    string
	storage string
}

func demoClusters() []*demoAPI {
	return []*demoAPI{
		{id: "vienna", name: "Wien · Produktion", tasks: map[string]demoTask{}, inventory: []Guest{
			{101, "db-primary", "pve-vie-01", "qemu", "running", false},
			{102, "reverse-proxy", "pve-vie-01", "lxc", "running", false},
			{104, "fileserver", "pve-vie-02", "qemu", "running", false},
			{108, "monitoring", "pve-vie-02", "lxc", "running", false},
			{112, "build-runner", "pve-vie-03", "qemu", "stopped", false},
			{115, "qa-snapshot", "pve-vie-03", "qemu", "running", false},
		}},
		{id: "graz", name: "Graz · Test", tasks: map[string]demoTask{}, inventory: []Guest{
			{101, "staging-api", "pve-grz-01", "qemu", "running", false},
			{201, "test-db", "pve-grz-02", "qemu", "stopped", false},
		}},
	}
}
func (d *demoAPI) guests(context.Context) ([]Guest, error) { return d.inventory, nil }
func (d *demoAPI) storages(_ context.Context, node string) ([]Storage, error) {
	result := []Storage{{"pbs-" + d.id, "pbs", true, true, "backup", true}}
	if node == "pve-vie-01" {
		result = append(result, Storage{"local", "dir", true, true, "backup", false})
	}
	return result, nil
}
func (d *demoAPI) start(_ context.Context, node string, guest int, storage string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	upid := fmt.Sprintf("UPID:%s:%016X:00000000:00000000:vzdump:%d:demo@pve:", node, now.UnixNano(), guest)
	d.tasks[upid] = demoTask{now, guest == 115, guest, node, storage}
	return upid, nil
}
func (d *demoAPI) status(_ context.Context, _, upid string) (TaskStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	task, ok := d.tasks[upid]
	if !ok {
		return TaskStatus{}, fmt.Errorf("Demo-Task wurde zurückgesetzt")
	}
	if time.Since(task.started) < 3*time.Second {
		return TaskStatus{Status: "running"}, nil
	}
	if task.fail {
		return TaskStatus{"stopped", "Simulierter Fehler beim Schreiben des Backups"}, nil
	}
	return TaskStatus{"stopped", "OK"}, nil
}
func (d *demoAPI) logs(_ context.Context, _, upid string, start int) ([]LogLine, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	task, ok := d.tasks[upid]
	if !ok {
		return nil, fmt.Errorf("Demo-Task wurde zurückgesetzt")
	}
	lines := []LogLine{
		{1, fmt.Sprintf("INFO: starting backup of guest %d on %s", task.guest, task.node)},
		{2, "INFO: backup mode: snapshot"},
		{3, "INFO: backup storage: " + task.storage},
	}
	if time.Since(task.started) >= 3*time.Second {
		if task.fail {
			lines = append(lines, LogLine{4, "ERROR: simulated storage write failure"})
		} else {
			lines = append(lines, LogLine{4, "INFO: Backup finished successfully"})
		}
	}
	return lines[min(start, len(lines)):], nil
}
