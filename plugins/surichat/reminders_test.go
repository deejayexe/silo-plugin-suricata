package main

import (
	"context"
	"database/sql"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestRemindersWaitRepeatAndStop(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unexpected authorization", 400)
			return
		}
		switch r.URL.Path {
		case "/api/v2/admin/users/42":
			w.Write([]byte(`{"id":"42","username":"lucia","role":"user","enabled":true,"email":"user@example.test"}`))
		case "/api/v2/admin/users":
			if r.URL.Query().Get("identity") != "support@example.test" {
				http.Error(w, "unexpected identity", 400)
				return
			}
			w.Write([]byte(`{"items":[{"id":"3","username":"operator","role":"admin","enabled":true,"email":"support@example.test"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	ctx := t.Context()
	cfg := settings{
		database: filepath.Join(t.TempDir(), "chat.sqlite"), serverURL: api.URL, apiKey: "test-key",
		mail: mailSettings{enabled: true, supportEmail: "support@example.test", from: "silo@example.test", host: "smtp.example.test"},
	}
	app := &support{config: cfg}
	db, _, err := app.storage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	created := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	_, err = db.ExecContext(ctx, `INSERT INTO message(user_id,sender,from_admin,body,sent_at) VALUES
		(42,'user',0,'help',?),(42,'admin',1,'reply',?),(42,'admin',1,'another reply',?)`,
		created.Format(time.RFC3339), created.Format(time.RFC3339), created.Add(time.Minute).Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	sent := map[string]int{}
	deliver := func(_ context.Context, mail mailSettings, to string, unread int) error {
		names := map[string]string{"user@example.test": "lucia", "support@example.test": "operator"}
		if mail.recipientName != names[to] {
			t.Errorf("recipient %s: got %q, want %q", to, mail.recipientName, names[to])
		}
		if _, exists := sent[to]; exists {
			t.Errorf("duplicate reminder for %s", to)
		}
		sent[to] = unread
		return nil
	}
	run := func(minutes int, want map[string]int) {
		t.Helper()
		clear(sent)
		if err := sendReminders(ctx, db, cfg, created.Add(time.Duration(minutes)*time.Minute), deliver); err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(sent, want) {
			t.Fatalf("at %d minutes got %v, want %v", minutes, sent, want)
		}
	}
	run(9, nil)
	run(10, map[string]int{"user@example.test": 1, "support@example.test": 1})
	run(11, map[string]int{"user@example.test": 1})
	run(20, nil)
	run(249, nil)
	run(250, map[string]int{"user@example.test": 1, "support@example.test": 1})
	run(251, map[string]int{"user@example.test": 1})
	run(500, nil)
	cfg.mail.firstMinutes = 3
	cfg.mail.repeatHours = 2
	_, err = db.ExecContext(ctx, `INSERT INTO message(user_id,sender,from_admin,body,sent_at) VALUES
        (42,'admin',1,'new reply',?)`, created.Add(501*time.Minute).Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	run(503, nil)
	run(504, map[string]int{"user@example.test": 1})
	run(623, nil)
	_, err = db.ExecContext(ctx, `INSERT INTO read_cursor(viewer_id,viewer_admin,user_id,last_id) VALUES (42,0,42,4)`)
	if err != nil {
		t.Fatal(err)
	}
	run(624, nil)
}

func TestReminderMigrationRetiresPreviouslyEligibleMessages(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `
		CREATE TABLE message(id INTEGER PRIMARY KEY, user_id INTEGER, from_admin INTEGER, sent_at TEXT);
		CREATE TABLE mail_reminder(user_id INTEGER PRIMARY KEY, attempted_at INTEGER);
		INSERT INTO message VALUES (1,42,1,'2026-11-19T12:00:00Z'), (2,42,1,'2026-11-19T12:10:01Z');
		INSERT INTO mail_reminder VALUES (42,unixepoch('2026-11-19T12:20:00Z'));`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateReminderCounts(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var prior, newer int
	err = db.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT attempts FROM mail_message_attempt WHERE message_id=1),0),
		COALESCE((SELECT attempts FROM mail_message_attempt WHERE message_id=2),0)`).Scan(&prior, &newer)
	if err != nil || prior != 2 || newer != 0 {
		t.Fatalf("migration yielded prior=%d newer=%d err=%v", prior, newer, err)
	}
}
