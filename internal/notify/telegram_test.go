package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFromEnvRequiresBothValues(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_CHAT_ID", "")
	if _, ok := FromEnv(); ok {
		t.Fatal("без переменных ожидали ok = false")
	}

	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	if _, ok := FromEnv(); ok {
		t.Fatal("без chat_id ожидали ok = false")
	}

	t.Setenv("TELEGRAM_CHAT_ID", "42")
	if _, ok := FromEnv(); !ok {
		t.Fatal("с обеими переменными ожидали ok = true")
	}
}

func TestSendPostsMessage(t *testing.T) {
	var gotPath string
	var got struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	tg := &Telegram{token: "secret-token", chatID: "42", baseURL: srv.URL, hc: srv.Client()}
	if err := tg.Send(context.Background(), "Новая заявка"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/botsecret-token/sendMessage") {
		t.Errorf("путь запроса = %q", gotPath)
	}
	if got.ChatID != "42" || got.Text != "Новая заявка" {
		t.Errorf("тело запроса: %+v", got)
	}
}

func TestSendErrorHidesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Internal error"})
	}))
	defer srv.Close()

	tg := &Telegram{token: "secret-token", chatID: "42", baseURL: srv.URL, hc: srv.Client()}
	err := tg.Send(context.Background(), "текст")
	if err == nil {
		t.Fatal("ожидали ошибку")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("ошибка раскрывает токен: %v", err)
	}
}

func TestSendNetworkErrorHidesToken(t *testing.T) {
	tg := &Telegram{token: "secret-token", chatID: "42", baseURL: "http://127.0.0.1:1", hc: http.DefaultClient}
	err := tg.Send(context.Background(), "текст")
	if err == nil {
		t.Fatal("ожидали ошибку сети")
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("ошибка раскрывает адрес с токеном: %v", err)
	}
}
