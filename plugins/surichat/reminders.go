package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"net"
	"net/mail"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const legacyReminderInterval = 10 * time.Minute

type mailSettings struct {
	firstMinutes, repeatHours                                  int
	recipientName                                              string
	enabled                                                    bool
	supportEmail, host, port, from, username, password, webURL string
	brand                                                      emailBrand
}

func (s *support) Run(ctx context.Context, _ *pb.RunScheduledTaskRequest) (*pb.RunScheduledTaskResponse, error) {
	s.mu.Lock()
	enabled := s.config.mail.enabled
	s.mu.Unlock()
	if !enabled {
		return &pb.RunScheduledTaskResponse{}, nil
	}

	db, cfg, err := s.storage(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.mail.enabled {
		if err := sendReminders(ctx, db, cfg, time.Now(), deliverReminder); err != nil {
			return nil, err
		}
	}
	return &pb.RunScheduledTaskResponse{}, nil
}

// Older installations only kept the latest attempt per recipient. Retire
// messages that were already eligible at that attempt to prevent more mail.
func migrateReminderCounts(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='mail_message_attempt')`).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		_, err = tx.ExecContext(ctx, `CREATE TABLE mail_message_attempt (
            message_id INTEGER PRIMARY KEY, attempts INTEGER NOT NULL CHECK(attempts BETWEEN 1 AND 2)
        );
        INSERT INTO mail_message_attempt(message_id,attempts)
        SELECT m.id,2 FROM message m JOIN mail_reminder d
        ON d.user_id=CASE WHEN m.from_admin=1 THEN m.user_id ELSE 0 END
        WHERE d.attempted_at>=unixepoch(m.sent_at)+?`, int64(legacyReminderInterval/time.Second))
		if err != nil {
			return err
		}
	}
	var timed bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('mail_message_attempt') WHERE name='attempted_at')`).Scan(&timed)
	if err != nil {
		return err
	}
	if !timed {
		if _, err = tx.ExecContext(ctx, `ALTER TABLE mail_message_attempt ADD COLUMN attempted_at INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		// Legacy releases only retain the latest timestamp per recipient. Use
		// it as a conservative estimate for any message still on attempt one.
		if _, err = tx.ExecContext(ctx, `UPDATE mail_message_attempt SET attempted_at=COALESCE((
            SELECT d.attempted_at FROM message m JOIN mail_reminder d
            ON d.user_id=CASE WHEN m.from_admin=1 THEN m.user_id ELSE 0 END
            WHERE m.id=mail_message_attempt.message_id),0) WHERE attempts=1`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// A shared support mailbox stops receiving reminders as soon as an
// administrator handles the message. Individual counters remain independent.
const pendingReminders = `
	SELECT m.id, CASE WHEN m.from_admin = 1 THEN m.user_id ELSE 0 END AS recipient
    FROM message m LEFT JOIN mail_message_attempt a ON a.message_id=m.id
    WHERE ((a.message_id IS NULL AND m.sent_at<=?)
        OR (a.attempts=1 AND a.attempted_at<=?)) AND (
		(m.from_admin = 1 AND NOT EXISTS (
			SELECT 1 FROM read_cursor r WHERE r.user_id=m.user_id
				AND r.viewer_id=m.user_id AND r.viewer_admin=0 AND r.last_id>=m.id
		)) OR
		(m.from_admin = 0 AND NOT EXISTS (
			SELECT 1 FROM read_cursor r WHERE r.user_id=m.user_id
				AND r.viewer_admin=1 AND r.last_id>=m.id
		))
	)`

func sendReminders(ctx context.Context, db *sql.DB, cfg settings, now time.Time, deliver func(context.Context, mailSettings, string, int) error) error {
	m, err := cfg.mail.validated()
	if err != nil {
		return err
	}
	firstCutoff := now.Add(-time.Duration(m.firstMinutes) * time.Minute).UTC().Format(time.RFC3339)
	secondCutoff := now.Add(-time.Duration(m.repeatHours) * time.Hour).Unix()
	rows, err := db.QueryContext(ctx, `WITH pending AS (`+pendingReminders+`)
		SELECT DISTINCT recipient FROM pending p LEFT JOIN mail_reminder d ON d.user_id=p.recipient
		WHERE d.attempted_at IS NULL OR d.attempted_at<=?
		ORDER BY d.attempted_at, recipient LIMIT 100`, firstCutoff, secondCutoff, now.Add(-time.Minute).Unix())
	if err != nil {
		return err
	}
	var recipients []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		recipients = append(recipients, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if len(recipients) > 0 {
		brandURL := cfg.serverURL
		if brandURL == "" {
			brandURL = m.webURL
		}
		m.brand = loadEmailBrand(ctx, brandURL)
	}
	var firstError error
	for _, id := range recipients {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := remindRecipient(ctx, db, cfg, m, id, now, firstCutoff, secondCutoff, deliver); err != nil && firstError == nil {
			firstError = err
		}
	}
	return firstError
}

func remindRecipient(ctx context.Context, db *sql.DB, cfg settings, m mailSettings, id int, now time.Time, firstCutoff string, secondCutoff int64, deliver func(context.Context, mailSettings, string, int) error) error {
	// Reserve the interval before talking to SMTP. If the SMTP result is
	// uncertain or the process exits, the next attempt is still rate-limited.
	res, err := db.ExecContext(ctx, `INSERT INTO mail_reminder(user_id,attempted_at) VALUES(?,?)
		ON CONFLICT(user_id) DO UPDATE SET attempted_at=excluded.attempted_at
		WHERE attempted_at<=?`, id, now.Unix(), now.Add(-time.Minute).Unix())
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil || count == 0 {
		return err
	}

	to := m.supportEmail
	if id != 0 {
		key := strconv.Itoa(id)
		users, err := directoryLookup(ctx, cfg, "/"+key)
		if err != nil {
			return errors.New("no se pudo consultar el correo de la cuenta en Silo")
		}
		if len(users) != 1 || users[0].ID != key || users[0].Role != "user" || !validMailbox(users[0].Email) {
			return nil
		}
		to = users[0].Email
		m.recipientName = users[0].Name
	} else if cfg.apiKey != "" {
		matches, err := directoryLookup(ctx, cfg, "?identity="+url.QueryEscape(to))
		if err == nil && len(matches) == 1 && matches[0].Role == "admin" && strings.EqualFold(matches[0].Email, to) {
			m.recipientName = matches[0].Name
		}
	}

	// Recheck after the directory lookup and reserve one attempt for each
	// included message before SMTP, including when its result is uncertain.
	result, err := db.ExecContext(ctx, `WITH pending AS (`+pendingReminders+`)
        INSERT INTO mail_message_attempt(message_id,attempts,attempted_at)
        SELECT id,1,? FROM pending WHERE recipient=?
        ON CONFLICT(message_id) DO UPDATE SET attempts=attempts+1,attempted_at=excluded.attempted_at`,
		firstCutoff, secondCutoff, now.Unix(), id)
	if err != nil {
		return err
	}
	reserved, err := result.RowsAffected()
	if err != nil || reserved == 0 {
		return err
	}
	return deliver(ctx, m, to, int(reserved))
}

func validMailbox(address string) bool {
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address {
		return false
	}
	for _, r := range address {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}

func (m mailSettings) validated() (mailSettings, error) {
	if m.firstMinutes == 0 {
		m.firstMinutes = 10
	}
	if m.repeatHours == 0 {
		m.repeatHours = 4
	}
	if m.firstMinutes < 1 || m.firstMinutes > 10080 || m.repeatHours < 1 || m.repeatHours > 168 {
		return m, errors.New("invalid email reminder delays")
	}
	if m.port == "" {
		m.port = "587"
	}
	port, err := strconv.Atoi(m.port)
	if m.host == "" || err != nil || port < 1 || port > 65535 {
		return m, errors.New("configura un servidor y puerto SMTP válidos")
	}
	if !validMailbox(m.from) || !validMailbox(m.supportEmail) {
		return m, errors.New("configura direcciones válidas de remitente y soporte")
	}
	if m.webURL != "" {
		u, err := url.Parse(m.webURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(m.webURL, "\r\n") {
			return m, errors.New("configura una URL pública de Silo válida")
		}
	}
	return m, nil
}

func deliverReminder(ctx context.Context, m mailSettings, to string, unread int) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	address := net.JoinHostPort(m.host, m.port)
	tlsConfig := &tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return errors.New("no se pudo conectar con SMTP")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return errors.New("no se pudo preparar la conexión SMTP")
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	var transport net.Conn = conn
	if m.port == "465" {
		transport = tls.Client(conn, tlsConfig)
	}
	client, err := smtp.NewClient(transport, m.host)
	if err != nil {
		return errors.New("SMTP no respondió correctamente")
	}
	defer client.Close()
	if m.port != "465" {
		ok, _ := client.Extension("STARTTLS")
		if !ok || client.StartTLS(tlsConfig) != nil {
			return errors.New("SMTP requiere STARTTLS con certificado válido")
		}
	}
	if m.username != "" {
		if err := client.Auth(smtp.PlainAuth("", m.username, m.password, m.host)); err != nil {
			return errors.New("SMTP rechazó la autenticación")
		}
	}
	if client.Mail(m.from) != nil || client.Rcpt(to) != nil {
		return errors.New("SMTP rechazó el remitente o destinatario")
	}
	data, err := client.Data()
	if err != nil {
		return errors.New("SMTP rechazó los datos del aviso")
	}
	if reminderMessage(data, m, to, unread) != nil || data.Close() != nil {
		return errors.New("SMTP no confirmó la recepción del aviso")
	}
	return nil
}
