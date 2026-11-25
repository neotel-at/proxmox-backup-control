package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var assets embed.FS

//go:embed VERSION
var versionFile string

func releaseVersion() string { return strings.TrimSpace(versionFile) }

type config struct {
	Listen       string      `json:"listen"`
	StateFile    string      `json:"state_file"`
	WebhookEnv   string      `json:"webhook_env"`
	SMTP         *smtpConfig `json:"smtp"`
	ScheduleZone string      `json:"schedule_timezone"`
	Clusters     []Cluster   `json:"clusters"`
}
type clusterInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type app struct {
	*runner
	auth         *authState
	catalog      []clusterInfo
	demo         bool
	scheduleZone string
}

var keyPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func readConfig(path string) (config, error) {
	var cfg config
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	return cfg, decodeJSON(f, &cfg)
}
func main() {
	if err := runMain(); err != nil {
		log.Fatal(err)
	}
}
func runMain() error {
	path := flag.String("config", "config.json", "Pfad zur JSON-Konfiguration")
	demo := flag.Bool("demo", false, "Lokale Vorschau mit simulierten Proxmox-Backups")
	initUser := flag.String("init-user", "", "Erstes GUI-Konto anlegen und beenden")
	resetUser := flag.String("reset-user", "", "Passwort eines GUI-Kontos zurücksetzen und beenden")
	version := flag.Bool("version", false, "Programmversion anzeigen")
	flag.Parse()
	if *version {
		fmt.Println(releaseVersion())
		return nil
	}
	if *initUser != "" && *resetUser != "" {
		return errors.New("nur eine Benutzeraktion angeben")
	}
	cfg := config{Listen: "127.0.0.1:8080", StateFile: "data/demo.json"}
	var err error
	if !*demo {
		cfg, err = readConfig(*path)
		if err != nil {
			return err
		}
		if cfg.Listen == "" {
			cfg.Listen = "127.0.0.1:8080"
		}
		if cfg.StateFile == "" {
			cfg.StateFile = "data/state.json"
		}
	}
	stored, err := openStore(cfg.StateFile)
	if err != nil {
		return err
	}
	defer stored.lock.Close()
	if *initUser != "" {
		return userCommand(stored, *initUser, false)
	}
	if *resetUser != "" {
		return userCommand(stored, *resetUser, true)
	}
	if len(stored.snapshot().Users) == 0 {
		if !*demo {
			return errors.New("kein GUI-Konto vorhanden. Mit -init-user einrichten")
		}
		user, err := newUser(demoPassword)
		if err != nil {
			return err
		}
		if err := stored.update(func(s *State) error { s.Users = map[string]User{"demo": user}; return nil }); err != nil {
			return err
		}
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil || (host != "localhost" && !net.ParseIP(host).IsLoopback()) {
		return errors.New("listen muss eine Loopback-Adresse mit Port sein")
	}
	worker := &runner{store: stored, clusters: map[string]backupAPI{}, poll: 3 * time.Second, wake: make(chan struct{}, 1)}
	if cfg.WebhookEnv != "" {
		worker.webhook = os.Getenv(cfg.WebhookEnv)
		u, err := url.Parse(worker.webhook)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return errors.New("Webhook-Umgebungsvariable muss eine gültige HTTPS-URL enthalten")
		}
	}
	if cfg.SMTP != nil {
		worker.mailer, err = newSMTP(*cfg.SMTP)
		if err != nil {
			return fmt.Errorf("smtp: %w", err)
		}
	}
	if cfg.ScheduleZone == "" {
		cfg.ScheduleZone = defaultScheduleZone
	}
	if _, err := scheduleLocation(cfg.ScheduleZone); err != nil {
		return fmt.Errorf("schedule_timezone: %w", err)
	}
	a := &app{runner: worker, auth: newAuth(), demo: *demo, scheduleZone: cfg.ScheduleZone}
	for _, cluster := range cfg.Clusters {
		if !keyPattern.MatchString(cluster.ID) || strings.TrimSpace(cluster.Name) == "" || worker.clusters[cluster.ID] != nil {
			return fmt.Errorf("ungültige oder doppelte Cluster-Konfiguration: %q", cluster.ID)
		}
		client, err := newPVE(cluster)
		if err != nil {
			return fmt.Errorf("Cluster %s: %w", cluster.ID, err)
		}
		worker.clusters[cluster.ID] = client
		a.catalog = append(a.catalog, clusterInfo{cluster.ID, cluster.Name})
	}
	if *demo {
		for _, demoCluster := range demoClusters() {
			worker.clusters[demoCluster.id] = demoCluster
			a.catalog = append(a.catalog, clusterInfo{demoCluster.id, demoCluster.name})
		}
		worker.poll = time.Second
	}
	if len(a.catalog) == 0 {
		return errors.New("mindestens einen Cluster konfigurieren")
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 90 * time.Second}
	var workers sync.WaitGroup
	workers.Add(3)
	go func() { defer workers.Done(); worker.loop(ctx) }()
	go func() { defer workers.Done(); worker.notifyLoop(ctx) }()
	go func() { defer workers.Done(); worker.scheduleLoop(ctx) }()
	serverError := make(chan error, 1)
	go func() { serverError <- srv.Serve(listener) }()
	log.Printf("PVE Backup GUI %s läuft auf http://%s", releaseVersion(), listener.Addr())
	select {
	case <-ctx.Done():
	case err = <-serverError:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if shutErr := srv.Shutdown(shutdown); shutErr != nil {
		_ = srv.Close()
	}
	workers.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)
	mux.HandleFunc("GET /api/session", a.sessionInfo)
	mux.HandleFunc("GET /api/users", a.listUsers)
	mux.HandleFunc("POST /api/users", a.saveUser)
	mux.HandleFunc("POST /api/users/{name}/password", a.saveUser)
	mux.HandleFunc("POST /api/users/{name}/delete", a.deleteUser)
	mux.HandleFunc("POST /api/history/purge", a.purgeRuns)
	mux.HandleFunc("POST /api/runs/{id}/delete", a.deleteRun)
	mux.HandleFunc("GET /api/dashboard", a.dashboard)
	mux.HandleFunc("GET /api/inventory/{cluster}", a.inventory)
	mux.HandleFunc("GET /api/storage/{cluster}", a.storage)
	mux.HandleFunc("POST /api/plans", a.savePlan)
	mux.HandleFunc("POST /api/plans/{id}/delete", a.deletePlan)
	mux.HandleFunc("POST /api/plans/{id}/runs", a.enqueue)
	mux.HandleFunc("POST /api/plans/{id}/schedule", a.setSchedule)
	mux.HandleFunc("POST /api/runs/{id}/cancel", a.cancelRun)
	mux.HandleFunc("POST /api/runs/{id}/review", a.reviewRun)
	mux.HandleFunc("GET /api/runs/{id}/logs/{step}", a.logs)
	web, _ := fs.Sub(assets, "web")
	static := http.FileServer(http.FS(web))
	mux.Handle("GET /", static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if !a.demo && r.URL.Path == "/api/session" {
			w.Header().Set("X-PVE-Login-HTTPS", "required")
		}
		if r.Method == "POST" {
			content, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if content != "application/json" || r.Header.Get("X-PVE-Request") != "1" {
				problem(w, 403, "Ungültige Herkunft der Anfrage")
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && !(r.Method == "POST" && r.URL.Path == "/api/login") {
			a.requireUser(w, r, mux)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func respond(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
func problem(w http.ResponseWriter, code int, message string) {
	respond(w, code, map[string]string{"error": message})
}
func decodeJSON(r io.Reader, out any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("zusätzliche JSON-Daten")
	}
	return nil
}
func body(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := decodeJSON(http.MaxBytesReader(w, r.Body, 64<<10), out); err != nil {
		problem(w, 400, "Ungültige Anfrage: "+err.Error())
		return false
	}
	return true
}
func (a *app) dashboard(w http.ResponseWriter, _ *http.Request) {
	s := a.store.snapshot()
	runs := slices.Clone(s.Runs)
	slices.Reverse(runs)
	respond(w, 200, map[string]any{"clusters": a.catalog, "plans": s.Plans, "runs": runs, "demo": a.demo, "webhook": a.webhook != "", "schedules": s.Schedules, "schedule_timezone": a.scheduleZone, "version": releaseVersion()})
}
func (a *app) inventory(w http.ResponseWriter, r *http.Request) {
	api := a.clusters[r.PathValue("cluster")]
	if api == nil {
		problem(w, 404, "Cluster nicht gefunden")
		return
	}
	guests, err := api.guests(r.Context())
	if err != nil {
		problem(w, 502, err.Error())
		return
	}
	respond(w, 200, guests)
}

func validTargets(ctx context.Context, api backupAPI, guests []Guest) ([]Storage, error) {
	nodes := map[string]bool{}
	var common []Storage
	for _, guest := range guests {
		if nodes[guest.Node] {
			continue
		}
		stores, err := api.storages(ctx, guest.Node)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", guest.Node, err)
		}
		if len(nodes) == 0 {
			common = slices.Clone(stores)
		} else {
			common = slices.DeleteFunc(common, func(candidate Storage) bool {
				return !slices.ContainsFunc(stores, func(s Storage) bool {
					return s.ID == candidate.ID && s.Type == candidate.Type && bool(s.Shared) && bool(candidate.Shared)
				})
			})
		}
		nodes[guest.Node] = true
	}
	slices.SortFunc(common, func(a, b Storage) int { return strings.Compare(a.ID, b.ID) })
	return common, nil
}
func (a *app) storage(w http.ResponseWriter, r *http.Request) {
	api := a.clusters[r.PathValue("cluster")]
	if api == nil {
		problem(w, 404, "Cluster nicht gefunden")
		return
	}
	nodes := r.URL.Query()["node"]
	if len(nodes) == 0 || len(nodes) > 100 {
		problem(w, 400, "Ein bis 100 Knoten erforderlich")
		return
	}
	guests := make([]Guest, 0, len(nodes))
	for _, n := range nodes {
		if !keyPattern.MatchString(n) {
			problem(w, 400, "Ungültiger Knotenname")
			return
		}
		guests = append(guests, Guest{Node: n})
	}
	targets, err := validTargets(r.Context(), api, guests)
	if err != nil {
		problem(w, 502, err.Error())
		return
	}
	respond(w, 200, targets)
}
func (a *app) savePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Cluster  string `json:"cluster"`
		Storage  string `json:"storage"`
		IDs      []int  `json:"vmids"`
		Continue bool   `json:"continue_on_error"`
	}
	if !body(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	api := a.clusters[req.Cluster]
	if api == nil || len(req.Name) == 0 || len([]rune(req.Name)) > 100 || !keyPattern.MatchString(req.Storage) || len(req.IDs) == 0 || len(req.IDs) > 1000 {
		problem(w, 400, "Name, Cluster, Backup-Ziel und ein bis 1.000 Gäste erforderlich")
		return
	}
	guests, err := api.guests(r.Context())
	if err != nil {
		problem(w, 502, err.Error())
		return
	}
	index := map[int]Guest{}
	for _, g := range guests {
		index[g.ID] = g
	}
	plan := Plan{ID: req.ID, Name: req.Name, Cluster: req.Cluster, Storage: req.Storage, Continue: req.Continue}
	for _, id := range req.IDs {
		guest, ok := index[id]
		if !ok || guest.Node == "" {
			problem(w, 400, fmt.Sprintf("Gast %d fehlt oder wurde doppelt gewählt", id))
			return
		}
		plan.Guests = append(plan.Guests, guest)
		delete(index, id)
	}
	targets, err := validTargets(r.Context(), api, plan.Guests)
	if err != nil {
		problem(w, 502, err.Error())
		return
	}
	target := slices.IndexFunc(targets, func(s Storage) bool { return s.ID == plan.Storage })
	if target < 0 {
		problem(w, 400, "Backup-Ziel steht nicht auf allen ausgewählten Knoten zur Verfügung")
		return
	}
	plan.StorageType = targets[target].Type
	if !targets[target].Shared {
		plan.TargetNode = plan.Guests[0].Node
	}
	err = a.store.update(func(s *State) error {
		if plan.ID == "" {
			plan.ID = newID()
			s.Plans = append(s.Plans, plan)
			return nil
		}
		for i := range s.Plans {
			if s.Plans[i].ID == plan.ID {
				s.Plans[i] = plan
				return nil
			}
		}
		return errMissing
	})
	if !handleWrite(w, err) {
		return
	}
	respond(w, 200, plan)
}
func newID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}

var errMissing = errors.New("Eintrag nicht gefunden")
var errConflict = errors.New("Aktion im aktuellen Status nicht möglich")

func handleWrite(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, errMissing):
		problem(w, 404, err.Error())
	case errors.Is(err, errConflict):
		problem(w, 409, err.Error())
	default:
		log.Printf("Zustand speichern: %v", err)
		problem(w, 500, "Änderung konnte nicht gespeichert werden")
	}
	return false
}
func (a *app) deletePlan(w http.ResponseWriter, r *http.Request) {
	err := a.store.update(func(s *State) error {
		for i, plan := range s.Plans {
			if plan.ID == r.PathValue("id") {
				s.Plans = slices.Delete(s.Plans, i, i+1)
				delete(s.Schedules, plan.ID)
				return nil
			}
		}
		return errMissing
	})
	if handleWrite(w, err) {
		respond(w, 200, struct{}{})
	}
}
func (a *app) enqueue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Due *time.Time `json:"due"`
	}
	if !body(w, r, &req) {
		return
	}
	due := time.Now().UTC()
	if req.Due != nil {
		if req.Due.Before(due) {
			problem(w, 400, "Der Starttermin muss in der Zukunft liegen")
			return
		}
		due = req.Due.UTC()
	}
	var job Run
	err := a.store.update(func(s *State) error {
		for _, plan := range s.Plans {
			if plan.ID != r.PathValue("id") {
				continue
			}
			job = newRun(plan, due, false)
			s.Runs = append(s.Runs, job)
			return nil
		}
		return errMissing
	})
	if handleWrite(w, err) {
		a.signal()
		respond(w, 202, job)
	}
}
func (a *app) cancelRun(w http.ResponseWriter, r *http.Request) {
	a.mutateRun(w, r, func(job *Run) error {
		if job.State != queued {
			return errConflict
		}
		job.State = "cancelled"
		now := time.Now().UTC()
		job.Finished = &now
		for i := range job.Steps {
			job.Steps[i].State = skipped
		}
		return nil
	})
}
func (a *app) reviewRun(w http.ResponseWriter, r *http.Request) {
	a.mutateRun(w, r, func(job *Run) error {
		if job.State != uncertain || job.Reviewed {
			return errConflict
		}
		job.Reviewed = true
		return nil
	})
}
func (a *app) mutateRun(w http.ResponseWriter, r *http.Request, fn func(*Run) error) {
	err := a.store.update(func(s *State) error {
		job := s.run(r.PathValue("id"))
		if job == nil {
			return errMissing
		}
		return fn(job)
	})
	if handleWrite(w, err) {
		a.signal()
		respond(w, 200, struct{}{})
	}
}
func (a *app) logs(w http.ResponseWriter, r *http.Request) {
	s := a.store.snapshot()
	job := s.run(r.PathValue("id"))
	i, err := strconv.Atoi(r.PathValue("step"))
	start := 0
	if raw := r.URL.Query().Get("start"); raw != "" {
		var parseErr error
		start, parseErr = strconv.Atoi(raw)
		if parseErr != nil || start < 0 || start > 10_000_000 {
			problem(w, 400, "Ungültiger Log-Offset")
			return
		}
	}
	if err != nil || job == nil || i < 0 || i >= len(job.Steps) {
		problem(w, 404, "Backup-Schritt nicht gefunden")
		return
	}
	step := job.Steps[i]
	if step.UPID == "" {
		problem(w, 404, "Für diesen Schritt liegt keine Task-ID vor")
		return
	}
	lines, archived := s.Logs[logKey(job.ID, i)]
	if archived && (step.LogError == "" || len(lines) == maxLogLines) {
		end := min(start+500, len(lines))
		lines = lines[min(start, end):end]
	} else {
		api := a.clusters[job.Plan.Cluster]
		var remote []LogLine
		var remoteErr error
		if api != nil {
			remote, remoteErr = api.logs(r.Context(), step.Node, step.UPID, start)
		} else {
			remoteErr = errors.New("Cluster fehlt in der Konfiguration")
		}
		switch {
		case remoteErr == nil:
			lines = remote
		case archived:
			end := min(start+500, len(lines))
			lines = lines[min(start, end):end]
		default:
			problem(w, 502, remoteErr.Error())
			return
		}
	}
	respond(w, 200, map[string]any{"lines": lines, "warning": step.LogError})
}
