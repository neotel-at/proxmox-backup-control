package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testPassword = "a unique test password"

func testLogin(t *testing.T, a *app) *http.Cookie {
	t.Helper()
	a.auth = newAuth()
	user, err := newUser(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.update(func(s *State) error {
		s.Users = map[string]User{"admin": user}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result := testRequest(t, a, nil, "POST", "/api/login", `{"username":"admin","password":"a unique test password"}`, 200)
	return result.Result().Cookies()[0]
}

func testRequest(t *testing.T, a *app, cookie *http.Cookie, method, path, payload string, status int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-PVE-Request", "1")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	result := httptest.NewRecorder()
	a.handler().ServeHTTP(result, request)
	if result.Code != status {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, result.Code, status, result.Body.String())
	}
	return result
}

func TestUserAdministrationAndSessionRevocation(t *testing.T) {
	a := &app{runner: testRunner(t, nil)}
	admin := testLogin(t, a)
	if !admin.Secure || !admin.HttpOnly || admin.Path != "/" || admin.SameSite != http.SameSiteStrictMode {
		t.Fatalf("missing cookie protections: %s", admin)
	}
	testRequest(t, a, nil, "GET", "/api/dashboard", "", 401)
	testRequest(t, a, nil, "POST", "/api/login", `{"username":"admin","password":"invalid"}`, 401)
	testRequest(t, a, admin, "POST", "/api/users", `{"username":"TestUser","password":"a unique test password"}`, 200)
	result := testRequest(t, a, admin, "GET", "/api/users", "", 200)
	if result.Body.String() != "[\"admin\",\"testuser\"]\n" {
		t.Fatalf("unexpected user list: %s", result.Body.String())
	}
	result = testRequest(t, a, nil, "POST", "/api/login", `{"username":"testuser","password":"a unique test password"}`, 200)
	second := result.Result().Cookies()[0]
	testRequest(t, a, second, "GET", "/api/dashboard", "", 200)
	testRequest(t, a, admin, "POST", "/api/users/testuser/password", `{"password":"another unique password"}`, 200)
	testRequest(t, a, second, "GET", "/api/session", "", 401)
	testRequest(t, a, admin, "POST", "/api/users/admin/delete", "{}", 409)
	result = testRequest(t, a, nil, "POST", "/api/login", `{"username":"testuser","password":"another unique password"}`, 200)
	second = result.Result().Cookies()[0]
	testRequest(t, a, admin, "POST", "/api/users/testuser/delete", "{}", 200)
	testRequest(t, a, second, "GET", "/api/session", "", 401)
	testRequest(t, a, admin, "POST", "/api/logout", "{}", 200)
	testRequest(t, a, admin, "GET", "/api/session", "", 401)
}

func TestHistoryDeletionAndPurge(t *testing.T) {
	a := &app{runner: testRunner(t, nil)}
	cookie := testLogin(t, a)
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -10)
	today := now.Add(-time.Hour)
	if err := a.store.update(func(s *State) error {
		s.Runs = []Run{
			{ID: "single", State: success, Finished: &old},
			{ID: "purge", State: failed, Finished: &old},
			{ID: "reviewed", State: uncertain, Finished: &old, Reviewed: true},
			{ID: "hold", State: uncertain, Finished: &old},
			{ID: "today", State: "cancelled", Finished: &today},
			{ID: "waiting", State: queued, Due: old},
			{ID: "active", State: running},
		}
		for _, run := range s.Runs {
			s.Logs[logKey(run.ID, 0)] = []LogLine{{Number: 1, Text: "sample"}}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	testRequest(t, a, nil, "POST", "/api/history/purge", `{"days":5}`, 401)
	for _, id := range []string{"active", "waiting", "hold"} {
		testRequest(t, a, cookie, "POST", fmt.Sprintf("/api/runs/%s/delete", id), "{}", 409)
	}
	testRequest(t, a, cookie, "POST", "/api/runs/single/delete", "{}", 200)
	testRequest(t, a, cookie, "POST", "/api/history/purge", `{"days":0}`, 400)
	response := testRequest(t, a, cookie, "POST", "/api/history/purge", `{"days":5}`, 200)
	var result map[string]int
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result["deleted"] != 2 {
		t.Fatalf("purge response: %s", response.Body.String())
	}
	snapshot := a.store.snapshot()
	for _, id := range []string{"single", "purge", "reviewed"} {
		if snapshot.run(id) != nil || snapshot.Logs[logKey(id, 0)] != nil {
			t.Fatalf("deleted run or log still present: %s", id)
		}
	}
	for _, id := range []string{"active", "waiting", "hold", "today"} {
		if snapshot.run(id) == nil || snapshot.Logs[logKey(id, 0)] == nil {
			t.Fatalf("retained run or log missing: %s", id)
		}
	}
}

func TestSessionHTTPSRequirement(t *testing.T) {
	for _, tc := range []struct {
		demo bool
		want string
	}{
		{false, "required"},
		{true, ""},
	} {
		a := &app{runner: testRunner(t, nil), demo: tc.demo}
		w := testRequest(t, a, nil, "GET", "/api/session", "", 401)
		if got := w.Header().Get("X-PVE-Login-HTTPS"); got != tc.want {
			t.Errorf("demo=%v: HTTPS requirement = %q, want %q", tc.demo, got, tc.want)
		}
	}
}
