package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/httproute"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	_ "modernc.org/sqlite"
)

//go:embed manifest.json index.html support.js support.css reminder.html brand
var assets embed.FS

// Development builds override the source manifest version through -ldflags.
var version string

type support struct {
	pb.UnimplementedHttpRoutesServer
	pb.UnimplementedScheduledTaskServer
	mu      sync.Mutex
	config  settings
	db      *sql.DB
	dbPath  string
	csrfKey [32]byte
}

type settings struct {
	database, serverURL, apiKey string
	mail                        mailSettings
}

func main() {
	app := &support{}
	if _, err := rand.Read(app.csrfKey[:]); err != nil {
		log.Fatal(err)
	}
	manifest, _ := assets.ReadFile("manifest.json")
	runtime.ServeManifestWithOptions(manifest, version, runtime.CapabilityServers{HttpRoutes: app, ScheduledTask: app}, runtime.WithConfigure(app.configure))
}

func (s *support) configure(_ context.Context, entries []*pb.ConfigEntry) error {
	cfg := settings{}
	for _, entry := range entries {
		fields := entry.GetValue().GetFields()
		value := func(key string) string { return strings.TrimSpace(fields[key].GetStringValue()) }
		switch entry.GetKey() {
		case "support":
			cfg.database, cfg.serverURL, cfg.apiKey = value("database"), value("server_url"), value("api_key")
		case "mail":
			cfg.mail = mailSettings{
				enabled: fields["enabled"].GetBoolValue(), supportEmail: value("support_email"),
				host: value("smtp_host"), port: value("smtp_port"), from: value("smtp_from"),
				username: value("smtp_user"), password: fields["smtp_password"].GetStringValue(),
				webURL:       value("web_url"),
				firstMinutes: int(fields["first_delay_minutes"].GetNumberValue()),
				repeatHours:  int(fields["repeat_delay_hours"].GetNumberValue()),
			}
		}
	}
	if cfg.database == "" {
		cfg.database = "/config/silo/soporte/chat.sqlite"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = cfg
	return nil
}

func (s *support) storage(ctx context.Context) (*sql.DB, settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.config
	if s.db != nil {
		if cfg.database != s.dbPath {
			return nil, cfg, errors.New("reinicia el plugin tras cambiar la ruta")
		}
		return s.db, cfg, nil
	}
	if !filepath.IsAbs(cfg.database) {
		return nil, cfg, errors.New("ruta de SQLite no absoluta")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.database), 0700); err != nil {
		return nil, cfg, err
	}
	file, err := os.OpenFile(cfg.database, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, cfg, err
	}
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return nil, cfg, err
	}
	if err = file.Close(); err != nil {
		return nil, cfg, err
	}
	dsn := url.URL{Scheme: "file", Path: cfg.database, RawQuery: "_pragma=busy_timeout%285000%29"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, cfg, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `
 CREATE TABLE IF NOT EXISTS conversation (
   user_id INTEGER PRIMARY KEY, username TEXT NOT NULL
 );
 CREATE TABLE IF NOT EXISTS message (
   id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL,
   sender TEXT NOT NULL, from_admin INTEGER NOT NULL, body TEXT NOT NULL, sent_at TEXT NOT NULL
 );
 CREATE INDEX IF NOT EXISTS message_conversation ON message(user_id,id);
 CREATE TABLE IF NOT EXISTS read_cursor (
   viewer_id INTEGER NOT NULL, viewer_admin INTEGER NOT NULL, user_id INTEGER NOT NULL,
   last_id INTEGER NOT NULL, PRIMARY KEY(viewer_id, viewer_admin, user_id)
 );
 CREATE INDEX IF NOT EXISTS read_cursor_conversation ON read_cursor(user_id, viewer_admin, last_id);
 CREATE TABLE IF NOT EXISTS mail_reminder (
   user_id INTEGER PRIMARY KEY, attempted_at INTEGER NOT NULL
 );`)
	if err != nil {
		db.Close()
		return nil, cfg, err
	}
	if err := migrateReminderCounts(ctx, db); err != nil {
		db.Close()
		return nil, cfg, err
	}
	s.db, s.dbPath = db, cfg.database
	return db, cfg, nil
}

func response(code int, kind string, body []byte) *pb.HandleHTTPResponse {
	return &pb.HandleHTTPResponse{StatusCode: int32(code), Headers: map[string]string{"Content-Type": kind, "Cache-Control": "private, no-store"}, Body: body}
}

func jsonResponse(code int, value any) *pb.HandleHTTPResponse {
	body, err := json.Marshal(value)
	if err != nil {
		return response(500, "text/plain; charset=utf-8", []byte("Error interno."))
	}
	return response(code, "application/json; charset=utf-8", body)
}

func fail(code int, message string) *pb.HandleHTTPResponse {
	return jsonResponse(code, map[string]string{"error": message})
}

func (s *support) token(viewer httproute.Viewer) string {
	mac := hmac.New(sha256.New, s.csrfKey[:])
	fmt.Fprintf(mac, "%d:%t:%s", viewer.UserID, viewer.Admin, viewer.ProfileID)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *support) Handle(ctx context.Context, req *pb.HandleHTTPRequest) (*pb.HandleHTTPResponse, error) {
	viewer, ok := httproute.ViewerOf(req)
	role := httproute.Header(req, httproute.HeaderUserRole)
	if !ok || (role != "user" && role != "admin") {
		return fail(401, "Abre Surichat desde tu sesión de Silo."), nil
	}
	path, method := req.GetPath(), req.GetMethod()
	if method == http.MethodGet {
		var name, mimeType string
		switch path {
		case "/support", "/support/":
			name, mimeType = "index.html", "text/html; charset=utf-8"
		case "/support.js":
			name, mimeType = "support.js", "application/javascript; charset=utf-8"
		case "/surichat.png":
			name, mimeType = "brand/surichat.png", "image/png"
		case "/support.css":
			name, mimeType = "support.css", "text/css; charset=utf-8"
		}
		if name != "" {
			body, err := assets.ReadFile(name)
			if err != nil {
				return fail(404, "Ruta desconocida."), nil
			}
			if name == "index.html" {
				if _, err := fs.Stat(assets, "brand/surichat.png"); err == nil {
					body = []byte(strings.Replace(string(body), "<h1>Surichat</h1>", `<h1><img class="surichat-logo" src="./surichat.png" width="512" height="473" alt="Surichat"></h1>`, 1))
				}
			}
			return response(200, mimeType, body), nil
		}
	}
	if (path == "/find" || path == "/directory") && method == http.MethodGet && !viewer.Admin {
		return fail(403, "Acceso reservado a administración."), nil
	}
	if !(method == http.MethodGet && (path == "/state" || path == "/find" || path == "/directory") || method == http.MethodPost && (path == "/send" || path == "/read")) {
		return fail(404, "Ruta desconocida."), nil
	}
	db, cfg, err := s.storage(ctx)
	if err != nil {
		log.Printf("soporte: almacenamiento: %v", err)
		return fail(503, "No se pudo abrir el historial. Comprueba la ruta y los permisos de SQLite."), nil
	}
	switch path {
	case "/find":
		return findUser(ctx, cfg, query(req, "identity")), nil
	case "/directory":
		return listUsers(ctx, cfg, query(req, "cursor")), nil
	case "/state":
		return s.state(ctx, db, viewer, query(req, "user"), query(req, "before")), nil
	case "/read":
		return s.markRead(ctx, db, viewer, req), nil
	default:
		return s.send(ctx, db, cfg, viewer, req), nil
	}
}

func query(req *pb.HandleHTTPRequest, key string) string {
	return req.GetQuery().GetFields()[key].GetStringValue()
}

type account struct {
	ID     string `json:"id"`
	Name   string `json:"username"`
	Unread int    `json:"unread,omitempty"`
}
type chatMessage struct {
	ID     int64  `json:"id"`
	Sender string `json:"sender"`
	Admin  bool   `json:"admin"`
	Body   string `json:"body"`
	At     string `json:"at"`
}

func (s *support) state(ctx context.Context, db *sql.DB, v httproute.Viewer, user, before string) *pb.HandleHTTPResponse {
	out := struct {
		Admin      bool          `json:"admin"`
		Token      string        `json:"token"`
		Accounts   []account     `json:"accounts"`
		Messages   []chatMessage `json:"messages"`
		More       bool          `json:"more"`
		Viewer     string        `json:"viewer"`
		Unread     int           `json:"unread"`
		ChatUnread int           `json:"chat_unread"`
	}{Admin: v.Admin, Token: s.token(v), Accounts: []account{}, Messages: []chatMessage{}, Viewer: strconv.Itoa(v.UserID)}
	target := v.UserID
	if v.Admin {
		target, _ = strconv.Atoi(user)
		rows, err := db.QueryContext(ctx, `SELECT CAST(c.user_id AS TEXT), c.username,
   (SELECT COUNT(*) FROM message m WHERE m.user_id=c.user_id AND m.from_admin=0
     AND m.id>COALESCE(r.last_id,0))
   FROM conversation c LEFT JOIN read_cursor r
     ON r.user_id=c.user_id AND r.viewer_id=? AND r.viewer_admin=1
   ORDER BY (SELECT MAX(m.id) FROM message m WHERE m.user_id=c.user_id) DESC`, v.UserID)
		if err != nil {
			return databaseError(err)
		}
		for rows.Next() {
			var a account
			if err := rows.Scan(&a.ID, &a.Name, &a.Unread); err != nil {
				rows.Close()
				return databaseError(err)
			}
			out.Accounts = append(out.Accounts, a)
			out.Unread += a.Unread
			if a.ID == user {
				out.ChatUnread = a.Unread
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return databaseError(err)
		}
		rows.Close()
	} else if user != "" && user != strconv.Itoa(v.UserID) {
		return fail(403, "Solo puedes ver tu conversación.")
	}
	if !v.Admin {
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message m
          LEFT JOIN read_cursor r ON r.viewer_id=m.user_id AND r.user_id=m.user_id AND r.viewer_admin=0
          WHERE m.user_id=? AND m.from_admin=1 AND m.id>COALESCE(r.last_id,0)`, target).Scan(&out.Unread)
		if err != nil {
			return databaseError(err)
		}
		out.ChatUnread = out.Unread
	}
	if target > 0 {
		cursor, _ := strconv.ParseInt(before, 10, 64)
		if cursor <= 0 {
			cursor = 1<<63 - 1
		}
		rows, err := db.QueryContext(ctx, `SELECT id,sender,from_admin,body,sent_at FROM message
   WHERE user_id=? AND id<? ORDER BY id DESC LIMIT 51`, target, cursor)
		if err != nil {
			return databaseError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var m chatMessage
			if err := rows.Scan(&m.ID, &m.Sender, &m.Admin, &m.Body, &m.At); err != nil {
				return databaseError(err)
			}
			out.Messages = append(out.Messages, m)
		}
		if err := rows.Err(); err != nil {
			return databaseError(err)
		}
		if len(out.Messages) > 50 {
			out.More = true
			out.Messages = out.Messages[:50]
		}
	}
	return jsonResponse(200, out)
}

func (s *support) markRead(ctx context.Context, db *sql.DB, v httproute.Viewer, req *pb.HandleHTTPRequest) *pb.HandleHTTPResponse {
	kind, _, _ := mime.ParseMediaType(httproute.Header(req, "Content-Type"))
	if kind != "application/json" {
		return fail(415, "Se esperaba una solicitud JSON.")
	}
	var input struct {
		User    string
		Token   string
		Through int64
	}
	if len(req.GetBody()) > 2048 || json.Unmarshal(req.GetBody(), &input) != nil || input.Through <= 0 {
		return fail(400, "Solicitud inválida.")
	}
	if !hmac.Equal([]byte(input.Token), []byte(s.token(v))) {
		return fail(403, "La sesión del chat cambió. Vuelve a cargar Surichat.")
	}
	target := v.UserID
	if v.Admin {
		target, _ = strconv.Atoi(input.User)
		if target <= 0 {
			return fail(400, "Selecciona un usuario.")
		}
	} else if input.User != "" && input.User != strconv.Itoa(target) {
		return fail(403, "Solo puedes ver tu conversación.")
	}
	result, err := db.ExecContext(ctx, `INSERT INTO read_cursor(viewer_id,viewer_admin,user_id,last_id)
        SELECT ?,?,user_id,id FROM message WHERE user_id=? AND id=?
        ON CONFLICT(viewer_id,viewer_admin,user_id) DO UPDATE SET last_id=MAX(last_id,excluded.last_id)`,
		v.UserID, v.Admin, target, input.Through)
	if err != nil {
		return databaseError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return databaseError(err)
	}
	if count == 0 {
		return fail(400, "Solicitud inválida.")
	}
	return jsonResponse(200, map[string]bool{"ok": true})
}

func (s *support) send(ctx context.Context, db *sql.DB, cfg settings, v httproute.Viewer, req *pb.HandleHTTPRequest) *pb.HandleHTTPResponse {
	kind, _, _ := mime.ParseMediaType(httproute.Header(req, "Content-Type"))
	if kind != "application/json" {
		return fail(415, "Se esperaba una solicitud JSON.")
	}
	var input struct {
		Body  string
		User  string
		Token string
	}
	if len(req.GetBody()) > 20000 || json.Unmarshal(req.GetBody(), &input) != nil {
		return fail(400, "Solicitud inválida.")
	}
	if !hmac.Equal([]byte(input.Token), []byte(s.token(v))) {
		return fail(403, "La sesión del chat cambió. Vuelve a cargar Surichat.")
	}
	input.Body = strings.TrimSpace(input.Body)
	if input.Body == "" || utf8.RuneCountInString(input.Body) > 2000 {
		return fail(400, "Escribe entre 1 y 2000 caracteres.")
	}
	sender := v.UserName
	if sender == "" {
		sender = fmt.Sprintf("Cuenta %d", v.UserID)
	}
	target, name := v.UserID, sender
	if v.Admin {
		var err error
		target, err = strconv.Atoi(input.User)
		if err != nil || target <= 0 || target == v.UserID {
			return fail(400, "Selecciona un usuario.")
		}
		err = db.QueryRowContext(ctx, "SELECT username FROM conversation WHERE user_id=?", target).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			user, err := lookup(ctx, cfg, "/"+strconv.Itoa(target))
			if err != nil || len(user) != 1 || user[0].ID != strconv.Itoa(target) {
				return fail(400, "No se pudo verificar la cuenta de Silo.")
			}
			name = user[0].Name
		} else if err != nil {
			return databaseError(err)
		}
	} else if input.User != "" && input.User != strconv.Itoa(v.UserID) {
		return fail(403, "Solo puedes escribir en tu conversación.")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO conversation(user_id,username) VALUES (?,?)
 ON CONFLICT(user_id) DO UPDATE SET username=CASE WHEN ? THEN conversation.username ELSE excluded.username END`, target, name, v.Admin); err != nil {
		return databaseError(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO message(user_id,sender,from_admin,body,sent_at) VALUES (?,?,?,?,?)", target, sender, v.Admin, input.Body, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return databaseError(err)
	}
	if err = tx.Commit(); err != nil {
		return databaseError(err)
	}
	return jsonResponse(201, map[string]bool{"ok": true})
}

func databaseError(err error) *pb.HandleHTTPResponse {
	log.Printf("soporte: consulta SQLite: %v", err)
	return fail(500, "No se pudo acceder al historial.")
}

func findUser(ctx context.Context, cfg settings, identity string) *pb.HandleHTTPResponse {
	identity = strings.TrimSpace(identity)
	if identity == "" || utf8.RuneCountInString(identity) > 320 {
		return fail(400, "Escribe el nombre de usuario o correo exacto.")
	}
	if cfg.apiKey == "" || cfg.serverURL == "" {
		return fail(503, "Configura la URL interna y la clave de Silo para iniciar chats.")
	}
	users, err := lookup(ctx, cfg, "?identity="+url.QueryEscape(identity)+"&limit=20")
	if err != nil {
		return fail(502, "No se pudo consultar las cuentas del servidor.")
	}
	return jsonResponse(200, users)
}

type directoryAccount struct {
	account
	Role    string `json:"role"`
	Enabled bool   `json:"enabled"`
	Email   string `json:"email"`
}

func lookup(ctx context.Context, cfg settings, suffix string) ([]account, error) {
	records, err := directoryLookup(ctx, cfg, suffix)
	if err != nil {
		return nil, err
	}
	accounts := []account{}
	for _, item := range records {
		if item.Role == "user" {
			accounts = append(accounts, item.account)
		}
	}
	return accounts, nil
}

func listUsers(ctx context.Context, cfg settings, cursor string) *pb.HandleHTTPResponse {
	if cfg.apiKey == "" || cfg.serverURL == "" {
		return fail(503, "Configura la URL interna y la clave de Silo para iniciar chats.")
	}
	params := url.Values{"limit": {"100"}}
	if cursor != "" {
		params.Set("cursor", cursor)
	}
	records, next, err := directoryPage(ctx, cfg, "?"+params.Encode())
	if err != nil {
		return fail(502, "No se pudo consultar las cuentas del servidor.")
	}
	users := []account{}
	for _, record := range records {
		if record.Role == "user" {
			users = append(users, record.account)
		}
	}
	return jsonResponse(200, struct {
		Items []account `json:"items"`
		Next  string    `json:"next_cursor,omitempty"`
	}{users, next})
}

func directoryLookup(ctx context.Context, cfg settings, suffix string) ([]directoryAccount, error) {
	users, _, err := directoryPage(ctx, cfg, suffix)
	return users, err
}

func directoryPage(ctx context.Context, cfg settings, suffix string) ([]directoryAccount, string, error) {
	base, err := url.Parse(cfg.serverURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || cfg.apiKey == "" {
		return nil, "", errors.New("configuración de API incompleta")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base.String(), "/")+"/api/v2/admin/users"+suffix, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	client := http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("API de cuentas: HTTP %d", resp.StatusCode)
	}
	var body struct {
		directoryAccount
		Items []directoryAccount `json:"items"`
		Page  struct {
			Next string `json:"next_cursor"`
		} `json:"page"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&body); err != nil {
		return nil, "", err
	}
	if body.ID != "" {
		body.Items = append(body.Items, body.directoryAccount)
	}
	accounts := []directoryAccount{}
	for _, item := range body.Items {
		if item.Enabled && item.ID != "" {
			accounts = append(accounts, item)
		}
	}
	return accounts, body.Page.Next, nil
}
