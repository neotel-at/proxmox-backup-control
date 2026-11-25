package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Cluster struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	TokenID   string `json:"token_id"`
	SecretEnv string `json:"secret_env"`
	CAFile    string `json:"ca_file"`
}

type Guest struct {
	ID       int     `json:"vmid"`
	Name     string  `json:"name"`
	Node     string  `json:"node"`
	Type     string  `json:"type"`
	Status   string  `json:"status"`
	Template pveBool `json:"template"`
}
type Storage struct {
	ID      string  `json:"storage"`
	Type    string  `json:"type"`
	Active  pveBool `json:"active"`
	Enabled pveBool `json:"enabled"`
	Content string  `json:"content"`
	Shared  pveBool `json:"shared"`
}

// Proxmox versions expose flags as JSON booleans or numbers.
type pveBool bool

func (flag *pveBool) UnmarshalJSON(value []byte) error {
	switch string(value) {
	case "1", "true":
		*flag = true
	case "0", "false", "null":
		*flag = false
	default:
		return fmt.Errorf("ungültiger Proxmox-Statuswert %q", value)
	}
	return nil
}

type TaskStatus struct {
	Status string `json:"status"`
	Exit   string `json:"exitstatus"`
}
type LogLine struct {
	Number int    `json:"n"`
	Text   string `json:"t"`
}

type pveClient struct {
	base  string
	token string
	http  *http.Client
}

func newPVE(cfg Cluster) (*pveClient, error) {
	base, err := url.Parse(cfg.URL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil ||
		base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || strings.Trim(base.Path, "/") != "" {
		return nil, errors.New("url muss eine HTTPS-Origin sein, z. B. https://pve.example:8006")
	}
	secret := os.Getenv(cfg.SecretEnv)
	if cfg.TokenID == "" || cfg.SecretEnv == "" || secret == "" || strings.ContainsAny(cfg.TokenID+secret, " \t\r\n") {
		return nil, errors.New("gültige Token-ID und befüllte Secret-Umgebungsvariable erforderlich")
	}
	tlsConfig, err := trustedTLS(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = tlsConfig
	return &pveClient{strings.TrimRight(base.String(), "/") + "/api2/json", "PVEAPIToken=" + cfg.TokenID + "=" + secret,
		&http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: noRedirect}}, nil
}

func trustedTLS(caFile string) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA-Datei enthält kein PEM-Zertifikat")
		}
		tlsConfig.RootCAs = roots
	}
	return tlsConfig, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

type pveError struct {
	code   int
	detail string
}

func (e *pveError) Error() string { return fmt.Sprintf("Proxmox HTTP %d: %s", e.code, e.detail) }

func (p *pveClient) request(ctx context.Context, method, path string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", p.token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var problem struct {
			Message string
			Errors  map[string]string
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&problem)
		var messages []string
		for key, value := range problem.Errors {
			messages = append(messages, key+": "+value)
		}
		slices.Sort(messages)
		return &pveError{resp.StatusCode, strings.TrimSpace(strings.Join(append([]string{resp.Status, problem.Message}, messages...), " "))}
	}
	var envelope struct{ Data json.RawMessage }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("Proxmox lieferte keine Daten")
	}
	return json.Unmarshal(envelope.Data, out)
}
func (p *pveClient) guests(ctx context.Context) ([]Guest, error) {
	var guests []Guest
	err := p.request(ctx, "GET", "/cluster/resources?type=vm", nil, &guests)
	guests = slices.DeleteFunc(guests, func(g Guest) bool {
		return g.ID <= 0 || bool(g.Template) || (g.Type != "qemu" && g.Type != "lxc")
	})
	slices.SortFunc(guests, func(a, b Guest) int { return a.ID - b.ID })
	return guests, err
}
func nodePath(node string) string       { return "/nodes/" + url.PathEscape(node) }
func taskPath(node, upid string) string { return nodePath(node) + "/tasks/" + url.PathEscape(upid) }
func (p *pveClient) storages(ctx context.Context, node string) ([]Storage, error) {
	var storages []Storage
	err := p.request(ctx, "GET", nodePath(node)+"/storage?content=backup&enabled=1", nil, &storages)
	storages = slices.DeleteFunc(storages, func(s Storage) bool {
		return !bool(s.Active) || !bool(s.Enabled) || !slices.Contains(strings.Split(s.Content, ","), "backup")
	})
	return storages, err
}

var errBackupNotStarted = errors.New("Proxmox startete keinen Backup-Task. Gast möglicherweise migriert.")

func (p *pveClient) start(ctx context.Context, node string, id int, storage string) (string, error) {
	var upid string
	err := p.request(ctx, "POST", nodePath(node)+"/vzdump", url.Values{
		"vmid":           {strconv.Itoa(id)},
		"storage":        {storage},
		"mode":           {"snapshot"},
		"remove":         {"0"},
		"notes-template": {"{{guestname}}"},
	}, &upid)
	if err == nil {
		if upid == "OK" {
			return "", errBackupNotStarted
		}
		parts := strings.Split(upid, ":")
		if len(parts) < 8 || parts[0] != "UPID" || parts[1] != node || parts[5] != "vzdump" || parts[6] != strconv.Itoa(id) {
			err = errors.New("Proxmox lieferte eine unerwartete Task-ID")
		}
	}
	return upid, err
}
func (p *pveClient) status(ctx context.Context, node, upid string) (TaskStatus, error) {
	var status TaskStatus
	err := p.request(ctx, "GET", taskPath(node, upid)+"/status", nil, &status)
	return status, err
}
func (p *pveClient) logs(ctx context.Context, node, upid string, start int) ([]LogLine, error) {
	var lines []LogLine
	err := p.request(ctx, "GET", taskPath(node, upid)+"/log?start="+strconv.Itoa(start)+"&limit=500", nil, &lines)
	return lines, err
}
