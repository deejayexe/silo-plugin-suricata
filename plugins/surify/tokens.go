package main

import (
	"crypto"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

var errApplicationToken = errors.New("Enter a Gotify application token: a legacy token or the private gtfya.… token copied when creating the application.")

// Gotify enhanced application credentials contain an Ed25519 seed, not a
// bearer token. Sign a fresh timestamp with Ed25519ph, matching Gotify's
// auth/token.go. Only the public key, timestamp and signature leave the plugin.
func applicationTokenSeed(token string) ([]byte, error) {
	if !strings.HasPrefix(token, "gtfya.") {
		return nil, errApplicationToken
	}
	seed, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(token, "gtfya."))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errApplicationToken
	}
	return seed, nil
}
func validApplicationToken(token string) bool {
	if strings.HasPrefix(token, "gtfy") && strings.Contains(token, ".") {
		_, err := applicationTokenSeed(token)
		return err == nil
	}
	return gotifyKeyPattern.MatchString(token)
}
func applicationTokenHeader(token string, now time.Time) (string, error) {
	if !validApplicationToken(token) {
		return "", errApplicationToken
	}
	if !strings.HasPrefix(token, "gtfya.") {
		return token, nil
	}
	seed, err := applicationTokenSeed(token)
	if err != nil {
		return "", err
	}
	key := ed25519.NewKeyFromSeed(seed)
	timestamp := strconv.FormatInt(now.Unix(), 10)
	digest := sha512.Sum512([]byte("iat=" + timestamp))
	signature, err := key.Sign(nil, digest[:], &ed25519.Options{Hash: crypto.SHA512})
	if err != nil {
		return "", errApplicationToken
	}
	return "gtfya." + base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) + "." + timestamp + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
