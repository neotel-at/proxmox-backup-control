package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

type smtpConfig struct {
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	TLSMode     string   `json:"tls_mode"`
	Username    string   `json:"username"`
	PasswordEnv string   `json:"password_env"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	CAFile      string   `json:"ca_file"`
}

type smtpMailer struct {
	host     string
	endpoint string
	tlsMode  string
	tls      *tls.Config
	auth     smtp.Auth
	from     *mail.Address
	to       []*mail.Address
}

func newSMTP(cfg smtpConfig) (*smtpMailer, error) {
	if cfg.Host == "" || strings.ContainsAny(cfg.Host, " \t\r\n/@[]\\") ||
		strings.Contains(cfg.Host, ":") && net.ParseIP(cfg.Host) == nil {
		return nil, errors.New("gültiger SMTP-Hostname ohne Port erforderlich")
	}
	var defaultPort int
	switch cfg.TLSMode {
	case "", "starttls":
		cfg.TLSMode = "starttls"
		defaultPort = 587
	case "tls":
		defaultPort = 465
	case "plain":
		defaultPort = 25
		if cfg.Username != "" || cfg.PasswordEnv != "" {
			return nil, errors.New("SMTP-Anmeldung benötigt STARTTLS oder TLS")
		}
		if cfg.CAFile != "" {
			return nil, errors.New("ca_file setzt STARTTLS oder TLS voraus")
		}
	default:
		return nil, errors.New("tls_mode muss starttls, tls oder plain sein")
	}
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, errors.New("ungültiger SMTP-Port")
	}
	from, err := smtpAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	if len(cfg.To) == 0 {
		return nil, errors.New("mindestens ein Eintrag in to ist erforderlich")
	}
	to := make([]*mail.Address, 0, len(cfg.To))
	for i, entry := range cfg.To {
		address, err := smtpAddress(entry)
		if err != nil {
			return nil, fmt.Errorf("to[%d]: %w", i, err)
		}
		to = append(to, address)
	}
	var tlsConfig *tls.Config
	if cfg.TLSMode != "plain" {
		tlsConfig, err = trustedTLS(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.ServerName = cfg.Host
	}

	var auth smtp.Auth
	if cfg.Username != "" || cfg.PasswordEnv != "" {
		password := os.Getenv(cfg.PasswordEnv)
		if cfg.Username == "" || strings.ContainsAny(cfg.Username, "\r\n\x00") || cfg.PasswordEnv == "" || password == "" {
			return nil, errors.New("username und befüllte password_env-Variable erforderlich")
		}
		auth = smtp.PlainAuth("", cfg.Username, password, cfg.Host)
	}
	return &smtpMailer{
		host: cfg.Host, endpoint: net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		tlsMode: cfg.TLSMode, tls: tlsConfig, auth: auth, from: from, to: to,
	}, nil
}

func smtpAddress(value string) (*mail.Address, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return nil, errors.New("ungültige E-Mail-Adresse")
	}
	address, err := mail.ParseAddress(value)
	if err != nil {
		return nil, errors.New("ungültige E-Mail-Adresse")
	}
	for _, char := range address.Address {
		if char <= ' ' || char >= 127 {
			return nil, errors.New("SMTP-E-Mail-Adresse muss druckbares ASCII verwenden")
		}
	}
	return address, nil
}

func (mailer *smtpMailer) send(ctx context.Context, job Run) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", mailer.endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}

	var connection net.Conn = conn
	if mailer.tlsMode == "tls" {
		encrypted := tls.Client(conn, mailer.tls)
		if err := encrypted.HandshakeContext(ctx); err != nil {
			return err
		}
		connection = encrypted
	}
	client, err := smtp.NewClient(connection, mailer.host)
	if err != nil {
		return err
	}
	defer client.Close()

	if mailer.tlsMode == "starttls" {
		if available, _ := client.Extension("STARTTLS"); !available {
			return errors.New("SMTP-Server bietet kein STARTTLS an")
		}
		if err := client.StartTLS(mailer.tls); err != nil {
			return err
		}
	}
	if mailer.auth != nil {
		if err := client.Auth(mailer.auth); err != nil {
			// Some servers echo parts of an AUTH exchange in their error response.
			return errors.New("SMTP-Anmeldung fehlgeschlagen")
		}
	}
	if err := client.Mail(mailer.from.Address); err != nil {
		return err
	}
	for _, recipient := range mailer.to {
		if err := client.Rcpt(recipient.Address); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(mailer.message(job)); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	// After DATA is accepted a failed QUIT must not trigger a resend.
	_ = client.Quit()
	return nil
}

func (mailer *smtpMailer) message(job Run) []byte {
	var message bytes.Buffer
	var recipients []string
	for _, recipient := range mailer.to {
		recipients = append(recipients, recipient.String())
	}
	subject := "PVE Backup fehlgeschlagen"
	if job.State == uncertain {
		subject = "PVE Backup: Prüfung erforderlich"
	}
	date := job.Due
	if job.Finished != nil {
		date = *job.Finished
	}
	planName := strings.Join(strings.Fields(job.Plan.Name), " ")
	domain := mailer.from.Address[strings.LastIndexByte(mailer.from.Address, '@')+1:]
	fmt.Fprintf(&message, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <pve-backup-%s@%s>\r\n",
		mailer.from.String(), strings.Join(recipients, ",\r\n "), mime.QEncoding.Encode("UTF-8", subject+" – "+planName),
		date.Format(time.RFC1123Z), job.ID, domain)
	message.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")

	body := quotedprintable.NewWriter(&message)
	fmt.Fprintf(body, "%s\r\n\r\nPlan: %s\r\nCluster: %s\r\nBackup-Ziel: %s\r\nLauf-ID: %s\r\nZeit: %s\r\n",
		subject, job.Plan.Name, job.Plan.Cluster, job.Plan.Storage, job.ID, date.UTC().Format(time.RFC3339))
	for _, step := range job.Steps {
		var result string
		switch step.State {
		case failed:
			result = "Fehler"
		case uncertain:
			result = "Prüfung erforderlich"
		default:
			continue
		}
		fmt.Fprintf(body, "\r\nGast %d %s – %s\r\n%s\r\n", step.Guest.ID, step.Guest.Name, result, step.Error)
	}
	_ = body.Close()
	return message.Bytes()
}
