package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSMTPDelivery(t *testing.T) {
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsServer.Close()
	caPath := filepath.Join(t.TempDir(), "smtp-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PVE_TEST_SMTP_PASSWORD", "fixture-password")
	for _, mode := range []string{"starttls", "tls", "plain"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			_, portText, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}
			cfg := smtpConfig{
				Host: "127.0.0.1", Port: port, TLSMode: mode, From: "Backup <backup@example.test>",
				To: []string{"ops@example.test", "team@example.test"},
			}
			if mode != "plain" {
				cfg.CAFile = caPath
				cfg.Username = "fixture"
				cfg.PasswordEnv = "PVE_TEST_SMTP_PASSWORD"
			}
			mailer, err := newSMTP(cfg)
			if err != nil {
				t.Fatal(err)
			}

			type delivery struct {
				message []byte
				err     error
			}
			inbox := make(chan delivery, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					inbox <- delivery{err: err}
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				message, err := receiveSMTP(conn, tlsServer.TLS, mode)
				inbox <- delivery{message, err}
			}()

			finished := time.Now().UTC()
			run := Run{
				ID: "test-run", State: failed, Finished: &finished,
				Plan:  Plan{Name: "Nächtlich", Cluster: "production", Storage: "pbs"},
				Steps: []Step{{Guest: Guest{ID: 115, Name: "app"}, State: failed, Error: "Snapshot fehlgeschlagen"}},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			worker := testRunner(t, nil)
			run.EmailNotice = &Notice{Next: finished, Status: queued}
			if err := worker.store.update(func(s *State) error {
				s.Runs = append(s.Runs, run)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			worker.deliverNotice(ctx, run, notificationChannel{
				name: "E-Mail", notice: func(run *Run) *Notice { return run.EmailNotice }, send: mailer.send,
			})
			snapshot := worker.store.snapshot()
			if notice := snapshot.run(run.ID).EmailNotice; notice.Status != success || notice.Attempts != 1 {
				t.Fatalf("SMTP-Versand nicht bestätigt: %+v", notice)
			}
			select {
			case received := <-inbox:
				if received.err != nil {
					t.Fatal(received.err)
				}
				message, err := mail.ReadMessage(bytes.NewReader(received.message))
				if err != nil {
					t.Fatal(err)
				}
				subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
				if err != nil || subject != "PVE Backup fehlgeschlagen – Nächtlich" {
					t.Fatalf("subject = %q, %v", subject, err)
				}
				body, err := io.ReadAll(message.Body)
				if err != nil || !bytes.Contains(body, []byte("Snapshot fehlgeschlagen")) {
					t.Fatalf("failed guest absent from body: %s, %v", body, err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func receiveSMTP(conn net.Conn, tlsConfig *tls.Config, mode string) ([]byte, error) {
	encrypted := mode == "tls"
	wrapTLS := func() error {
		secured := tls.Server(conn, tlsConfig)
		if err := secured.Handshake(); err != nil {
			return err
		}
		conn = secured
		encrypted = true
		return nil
	}
	if encrypted {
		if err := wrapTLS(); err != nil {
			return nil, err
		}
	}
	protocol := textproto.NewConn(conn)
	if err := protocol.PrintfLine("220 fixture ESMTP"); err != nil {
		return nil, err
	}
	var message []byte
	recipients := 0
	authenticated := false
	for {
		line, err := protocol.ReadLine()
		if err != nil {
			return nil, err
		}
		var reply string
		switch {
		case strings.HasPrefix(line, "EHLO "):
			reply = "250-fixture\r\n250 STARTTLS"
			if encrypted {
				reply = "250-fixture\r\n250 AUTH PLAIN"
			}
		case line == "STARTTLS" && !encrypted && mode == "starttls":
			if err := protocol.PrintfLine("220 Ready"); err != nil {
				return nil, err
			}
			if err := wrapTLS(); err != nil {
				return nil, err
			}
			protocol = textproto.NewConn(conn)
			continue
		case strings.HasPrefix(line, "AUTH PLAIN ") && encrypted:
			auth, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
			if err != nil || string(auth) != "\x00fixture\x00fixture-password" {
				return nil, errors.New("unexpected SMTP credentials")
			}
			authenticated = true
			reply = "235 authenticated"
		case line == "MAIL FROM:<backup@example.test>" && (authenticated || mode == "plain"):
			reply = "250 sender accepted"
		case (line == "RCPT TO:<ops@example.test>" || line == "RCPT TO:<team@example.test>") && (authenticated || mode == "plain"):
			recipients++
			reply = "250 recipient accepted"
		case line == "DATA" && recipients == 2:
			if err := protocol.PrintfLine("354 go ahead"); err != nil {
				return nil, err
			}
			message, err = protocol.ReadDotBytes()
			if err != nil {
				return nil, err
			}
			reply = "250 accepted"
		case line == "QUIT" && message != nil:
			return message, protocol.PrintfLine("221 bye")
		default:
			return nil, fmt.Errorf("unexpected SMTP command: %s", line)
		}
		if err := protocol.PrintfLine("%s", reply); err != nil {
			return nil, err
		}
	}
}

func TestPlainSMTPRejectsCredentialsAndUsesPort25(t *testing.T) {
	cfg := smtpConfig{TLSMode: "plain", Host: "smtp.example.test", From: "backup@example.test", To: []string{"ops@example.test"}}
	mailer, err := newSMTP(cfg)
	if err != nil || mailer.endpoint != "smtp.example.test:25" {
		t.Fatalf("plain SMTP endpoint: %+v, %v", mailer, err)
	}
	for _, bad := range []smtpConfig{
		{TLSMode: "plain", Host: cfg.Host, Username: "backup"},
		{TLSMode: "plain", Host: cfg.Host, PasswordEnv: "PVE_TEST_SMTP_PASSWORD"},
	} {
		if _, err := newSMTP(bad); err == nil || !strings.Contains(err.Error(), "SMTP-Anmeldung") {
			t.Fatalf("credentials should be rejected: %v", err)
		}
	}
}

func TestEmailNoticeRetriesWithoutChangingWebhook(t *testing.T) {
	worker := testRunner(t, nil)
	now := time.Now().UTC()
	if err := worker.store.update(func(s *State) error {
		s.Runs = append(s.Runs, Run{ID: "run", State: failed,
			EmailNotice: &Notice{Status: queued, Next: now}, Notice: &Notice{Status: success, Attempts: 1}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	channel := notificationChannel{
		name: "E-Mail", notice: func(run *Run) *Notice { return run.EmailNotice },
		send: func(context.Context, Run) error { return errors.New("fixture relay unavailable") },
	}
	state := worker.store.snapshot()
	worker.deliverNotice(context.Background(), *state.run("run"), channel)
	state = worker.store.snapshot()
	job := state.run("run")
	if job.EmailNotice.Status != queued || job.EmailNotice.Attempts != 1 || !job.EmailNotice.Next.After(now) ||
		job.Notice.Status != success || job.Notice.Attempts != 1 {
		t.Fatalf("unexpected notification state: email=%+v webhook=%+v", job.EmailNotice, job.Notice)
	}
}
