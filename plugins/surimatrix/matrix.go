package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	_ "github.com/mattn/go-sqlite3"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type matrixClient struct {
	cli    *mautrix.Client
	crypto *cryptohelper.CryptoHelper
	room   id.RoomID
	dir    string
}

func httpsURL(s string) error {
	u, e := url.Parse(s)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("An HTTPS URL without embedded credentials is required")
	}
	return nil
}
func withMatrix(ctx context.Context, c config, fn func(*matrixClient) error) error {
	if err := httpsURL(c.Homeserver); err != nil {
		return err
	}
	if !filepath.IsAbs(c.StateDir) || c.AccessToken == "" || c.DeviceID == "" || !strings.HasPrefix(c.UserID, "@") || !strings.HasPrefix(c.RoomID, "!") {
		return errors.New("Set the Matrix account, access token, device ID, internal room ID and an absolute persistent directory")
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(c.StateDir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("Another Surimatrix instance is using this session")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	identity := c.Homeserver + "\n" + c.UserID + "\n" + c.DeviceID + "\n" + c.RoomID
	identityPath := filepath.Join(c.StateDir, "identity")
	stored, err := os.ReadFile(identityPath)
	if os.IsNotExist(err) {
		err = atomicWrite(identityPath, []byte(identity))
	} else if err == nil && string(stored) != identity {
		return errors.New("The Matrix identity or room changed; use a different directory for a new session")
	}
	if err != nil {
		return err
	}
	keyPath := filepath.Join(c.StateDir, "pickle.key")
	key, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return err
		}
		err = atomicWrite(keyPath, key)
	}
	if err != nil {
		return err
	}
	cli, err := mautrix.NewClient(c.Homeserver, id.UserID(c.UserID), c.AccessToken)
	if err != nil {
		return err
	}
	cli.DeviceID = id.DeviceID(c.DeviceID)
	who, err := cli.Whoami(ctx)
	if err != nil {
		return fmt.Errorf("Matrix whoami: %w", err)
	}
	if who.UserID != cli.UserID || who.DeviceID != cli.DeviceID {
		return errors.New("The token does not match the configured Matrix account and device")
	}
	helper, err := cryptohelper.NewCryptoHelper(cli, key, filepath.Join(c.StateDir, "crypto.db"))
	if err != nil {
		return err
	}
	if err = helper.Init(ctx); err != nil {
		return fmt.Errorf("Initialize encryption: %w", err)
	}
	defer helper.Close()
	cli.Crypto = helper
	// Persist the sync token only after processing keys. The next sync acknowledges
	// to-device messages; room state is refreshed explicitly before each send.
	syncPath := filepath.Join(c.StateDir, "sync.json")
	var since string
	if err = loadJSON(syncPath, &since); err != nil {
		return err
	}
	resp, err := cli.SyncRequest(ctx, 0, since, "", false, event.PresenceOffline)
	if err != nil {
		return fmt.Errorf("Sync: %w", err)
	}
	if err = cli.Syncer.ProcessResponse(ctx, resp, since); err != nil {
		return err
	}
	if err = saveJSON(syncPath, resp.NextBatch); err != nil {
		return err
	}
	room := id.RoomID(c.RoomID)
	state, err := cli.State(ctx, room)
	if err != nil {
		return fmt.Errorf("Read room state: %w", err)
	}
	member := state[event.StateMember][c.UserID]
	if member == nil || member.Content.AsMember().Membership != event.MembershipJoin {
		return errors.New("The bot must join the configured room")
	}
	var enc event.EncryptionEventContent
	if err = cli.StateEvent(ctx, room, event.StateEncryption, "", &enc); err != nil {
		return errors.New("Unable to verify room encryption")
	}
	if enc.Algorithm != id.AlgorithmMegolmV1 {
		return errors.New("Surimatrix requires a Megolm-encrypted room")
	}
	// Explicitly encrypt every event: never fall back to plaintext if state is absent.
	return fn(&matrixClient{cli: cli, crypto: helper, room: room, dir: c.StateDir})
}
func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".surimatrix-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func txn(key string) string { s := sha256.Sum256([]byte(key)); return hex.EncodeToString(s[:]) }
func (m *matrixClient) send(ctx context.Context, key, text string, data []byte, mime string) error {
	content := event.MessageEventContent{MsgType: event.MsgText, Body: text}
	if len(data) > 0 {
		file := attachment.NewEncryptedFile()
		ciphertext := file.Encrypt(data)
		uploaded, err := m.cli.UploadBytes(ctx, ciphertext, "application/octet-stream")
		if err != nil {
			return fmt.Errorf("Upload encrypted poster: %w", err)
		}
		content.MsgType = event.MsgImage
		content.FileName = "poster" + map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[mime]
		content.File = &event.EncryptedFileInfo{EncryptedFile: *file, URL: uploaded.ContentURI.CUString()}
		content.Info = &event.FileInfo{MimeType: mime, Size: len(data)}
	}
	encrypted, err := m.crypto.Encrypt(ctx, m.room, event.EventMessage, &content)
	if err != nil {
		return fmt.Errorf("Encrypt message: %w", err)
	}
	_, err = m.cli.SendMessageEvent(ctx, m.room, event.EventEncrypted, encrypted, mautrix.ReqSendEvent{TransactionID: txn(key)})
	return err
}
func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func saveJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicWrite(path, b)
}
