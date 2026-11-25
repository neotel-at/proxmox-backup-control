package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/term"
)

type User struct {
	Salt   []byte `json:"salt"`
	Digest []byte `json:"digest"`
}
type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

const (
	sessionLifetime = 12 * time.Hour
	demoPassword    = "pve-backup-demo"
	maxPasswordSize = 256
)

func normalizeUsername(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func newUser(password string) (User, error) {
	if utf8.RuneCountInString(password) < 15 || len(password) > maxPasswordSize {
		return User{}, errors.New("Passwort muss mindestens 15 Zeichen und höchstens 256 Bytes haben")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return User{}, err
	}
	return User{Salt: salt, Digest: passwordDigest(password, salt)}, nil
}
func passwordDigest(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
}

func userCommand(s *store, username string, reset bool) error {
	username = normalizeUsername(username)
	if !keyPattern.MatchString(username) {
		return errors.New("ungültiger Benutzername")
	}
	users := s.snapshot().Users
	if !reset && len(users) != 0 {
		return errors.New("Ersteinrichtung ist bereits abgeschlossen")
	}
	if reset && users[username].Salt == nil {
		return errMissing
	}
	password, err := promptPassword("Neues Passwort")
	if err != nil {
		return err
	}
	defer clear(password)
	confirmation, err := promptPassword("Passwort wiederholen")
	if err != nil {
		return err
	}
	defer clear(confirmation)
	if !bytes.Equal(password, confirmation) {
		return errors.New("Passwörter stimmen nicht überein")
	}
	user, err := newUser(string(password))
	if err != nil {
		return err
	}
	return s.update(func(state *State) error {
		if state.Users == nil {
			state.Users = make(map[string]User)
		}
		state.Users[username] = user
		return nil
	})
}

func promptPassword(label string) ([]byte, error) {
	fmt.Fprint(os.Stderr, label+": ")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return password, err
}

type session struct {
	username string
	salt     []byte
	expires  time.Time
}
type authState struct {
	mu       sync.Mutex
	sessions map[string]session
	window   time.Time
	attempts map[string]int
	total    int
	slots    chan struct{}
}

func newAuth() *authState {
	return &authState{sessions: make(map[string]session), slots: make(chan struct{}, 2)}
}
func (auth *authState) allowLogin(username string, now time.Time) bool {
	auth.mu.Lock()
	defer auth.mu.Unlock()
	if !now.Before(auth.window) {
		auth.window = now.Add(time.Minute)
		auth.total = 0
		auth.attempts = make(map[string]int)
	}
	if auth.total >= 30 || auth.attempts[username] >= 8 {
		return false
	}
	auth.total++
	auth.attempts[username]++
	return true
}

func (a *app) cookieName() string {
	if a.demo {
		return "pve_demo_session"
	}
	return "__Host-pve_session"
}
func (a *app) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: token, Path: "/", HttpOnly: true,
		Secure: !a.demo, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

func (a *app) authenticated(r *http.Request) string {
	cookie, err := r.Cookie(a.cookieName())
	if err != nil || len(cookie.Value) != 64 {
		return ""
	}
	a.auth.mu.Lock()
	s, ok := a.auth.sessions[cookie.Value]
	if ok && !time.Now().Before(s.expires) {
		delete(a.auth.sessions, cookie.Value)
		ok = false
	}
	a.auth.mu.Unlock()
	if !ok || !bytes.Equal(a.store.snapshot().Users[s.username].Salt, s.salt) {
		return ""
	}
	return s.username
}

type userContextKey struct{}

func (a *app) requireUser(w http.ResponseWriter, r *http.Request, next http.Handler) {
	username := a.authenticated(r)
	if username == "" {
		problem(w, 401, "Bitte anmelden")
		return
	}
	ctx := context.WithValue(r.Context(), userContextKey{}, username)
	next.ServeHTTP(w, r.WithContext(ctx))
}
func requester(r *http.Request) string {
	return r.Context().Value(userContextKey{}).(string)
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if !body(w, r, &req) {
		return
	}
	username := normalizeUsername(req.Username)
	if !keyPattern.MatchString(username) || len(req.Password) > maxPasswordSize {
		problem(w, 401, "Benutzername oder Passwort ungültig")
		return
	}
	if !a.auth.allowLogin(username, time.Now()) {
		w.Header().Set("Retry-After", "60")
		problem(w, 429, "Zu viele Anmeldeversuche. Bitte in einer Minute erneut versuchen.")
		return
	}
	if !a.acquireHashSlot(w) {
		return
	}
	defer func() { <-a.auth.slots }()
	user, exists := a.store.snapshot().Users[username]
	if !exists {
		// Spend the same hashing work for unknown accounts.
		user = User{make([]byte, 16), make([]byte, 32)}
	}
	digest := passwordDigest(req.Password, user.Salt)
	if subtle.ConstantTimeCompare(digest, user.Digest) != 1 || !exists {
		problem(w, 401, "Benutzername oder Passwort ungültig")
		return
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		log.Printf("Sitzung erstellen: %v", err)
		problem(w, 500, "Sitzung konnte nicht erstellt werden")
		return
	}
	token := hex.EncodeToString(random[:])
	now := time.Now()
	if !bytes.Equal(a.store.snapshot().Users[username].Salt, user.Salt) {
		problem(w, 401, "Bitte erneut anmelden")
		return
	}
	a.auth.mu.Lock()
	for key, s := range a.auth.sessions {
		if !now.Before(s.expires) {
			delete(a.auth.sessions, key)
		}
	}
	if previous, err := r.Cookie(a.cookieName()); err == nil {
		delete(a.auth.sessions, previous.Value)
	}
	a.auth.sessions[token] = session{username, user.Salt, now.Add(sessionLifetime)}
	a.auth.mu.Unlock()
	a.setSessionCookie(w, token, int(sessionLifetime.Seconds()))
	respond(w, 200, map[string]string{"username": username})
}
func (a *app) acquireHashSlot(w http.ResponseWriter) bool {
	select {
	case a.auth.slots <- struct{}{}:
		return true
	default:
		problem(w, 429, "Bitte in wenigen Sekunden erneut versuchen")
		return false
	}
}
func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(a.cookieName()); err == nil {
		a.auth.mu.Lock()
		delete(a.auth.sessions, cookie.Value)
		a.auth.mu.Unlock()
	}
	a.setSessionCookie(w, "", -1)
	respond(w, 200, struct{}{})
}
func (a *app) sessionInfo(w http.ResponseWriter, r *http.Request) {
	respond(w, 200, map[string]string{"username": requester(r)})
}
func (a *app) listUsers(w http.ResponseWriter, _ *http.Request) {
	users := a.store.snapshot().Users
	names := make([]string, 0, len(users))
	for name := range users {
		names = append(names, name)
	}
	slices.Sort(names)
	respond(w, 200, names)
}
func (a *app) saveUser(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if !body(w, r, &req) {
		return
	}
	target := r.PathValue("name")
	name := normalizeUsername(req.Username)
	if target != "" {
		name = target
	}
	if !keyPattern.MatchString(name) {
		problem(w, 400, "Benutzername enthält ungültige Zeichen")
		return
	}
	if !a.acquireHashSlot(w) {
		return
	}
	defer func() { <-a.auth.slots }()
	user, err := newUser(req.Password)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	err = a.store.update(func(s *State) error {
		_, exists := s.Users[name]
		if target == "" && exists {
			return errConflict
		}
		if target != "" && !exists {
			return errMissing
		}
		s.Users[name] = user
		return nil
	})
	if handleWrite(w, err) {
		respond(w, 200, struct{}{})
	}
}
func (a *app) deleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == requester(r) {
		problem(w, 409, "Das eigene Konto kann nicht gelöscht werden")
		return
	}
	err := a.store.update(func(s *State) error {
		if _, exists := s.Users[name]; !exists {
			return errMissing
		}
		if len(s.Users) < 2 {
			return errConflict
		}
		delete(s.Users, name)
		return nil
	})
	if handleWrite(w, err) {
		respond(w, 200, struct{}{})
	}
}
