package http

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLoginPage_Renders(t *testing.T) {
	a := setupTestApp(t)
	handler := handleLoginPage(a)

	req := httptest.NewRequest("GET", "/admin/login", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("login page: status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Admin Login") {
		t.Error("login page should contain form")
	}
}

func TestLoginPage_ShowsError(t *testing.T) {
	a := setupTestApp(t)
	handler := handleLoginPage(a)

	req := httptest.NewRequest("GET", "/admin/login?error=bad+credentials", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "bad credentials") {
		t.Error("login page should display error message")
	}
}

func TestLoginPage_EscapesXSS(t *testing.T) {
	a := setupTestApp(t)
	handler := handleLoginPage(a)

	req := httptest.NewRequest("GET", `/admin/login?error=<script>alert(1)</script>`, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "<script>") {
		t.Error("login page should escape HTML in error parameter")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("login page should contain escaped version of script tag")
	}
}

func TestLogin_InvalidCredentials(t *testing.T) {
	a := setupTestApp(t)
	handler := handleLogin(a)

	form := url.Values{"email": {"wrong@example.com"}, "password": {"wrong"}}
	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("invalid login: status = %d, want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Errorf("should redirect to login with error, got %s", loc)
	}
}

func TestLogin_EmptyFields(t *testing.T) {
	a := setupTestApp(t)
	handler := handleLogin(a)

	form := url.Values{"email": {""}, "password": {""}}
	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("empty fields: status = %d, want 303", w.Code)
	}
}

func TestLogout_ClearsCookie(t *testing.T) {
	a := setupTestApp(t)
	handler := handleLogout(a)

	req := httptest.NewRequest("POST", "/admin/logout", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "fake-session-id"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("logout: status = %d, want 303", w.Code)
	}

	// Check cookie is cleared
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "session" && c.MaxAge < 0 {
			found = true
		}
	}
	if !found {
		t.Error("logout should clear session cookie")
	}
}

func TestLogin_ThrottlesRepeatedFailures(t *testing.T) {
	a := setupTestApp(t)
	handler := handleLogin(a)

	form := url.Values{"email": {"wrong@example.com"}, "password": {"wrong"}}
	for i := range maxFailedLoginAttempts {
		req := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form.Encode()))
		req.RemoteAddr = "203.0.113.10:12345"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusSeeOther {
			t.Fatalf("attempt %d: status = %d, want 303", i+1, w.Code)
		}
	}

	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form.Encode()))
	req.RemoteAddr = "203.0.113.10:54321"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("throttled attempt: status = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "error=too+many+attempts") {
		t.Fatalf("throttled attempt should redirect with too many attempts error, got %q", loc)
	}
}
