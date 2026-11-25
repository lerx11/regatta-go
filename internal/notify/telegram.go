// Package notify — уведомления владельцу сайта о новых заявках.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Telegram — минимальный клиент Bot API для одного личного чата.
type Telegram struct {
	token   string
	chatID  string
	baseURL string
	hc      *http.Client
}

// FromEnv читает TELEGRAM_BOT_TOKEN и TELEGRAM_CHAT_ID.
// ok == false, если задана только одна переменная или ни одной:
// сервис продолжает работать, заявки сохраняются без уведомлений.
func FromEnv() (*Telegram, bool) {
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	chatID := strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
	if token == "" || chatID == "" {
		return nil, false
	}
	return &Telegram{
		token:   token,
		chatID:  chatID,
		baseURL: "https://api.telegram.org",
		hc:      &http.Client{Timeout: 6 * time.Second},
	}, true
}

// Send отправляет одно сообщение владельцу.
//
// Ошибки не содержат токен и текст сообщения с контактами участника:
// URL с токеном никогда не попадает в текст ошибки.
func (t *Telegram) Send(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":                  t.chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	})
	if err != nil {
		return errors.New("telegram: не удалось собрать запрос")
	}

	url := t.baseURL + "/bot" + t.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return errors.New("telegram: не удалось собрать запрос")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.hc.Do(req)
	if err != nil {
		// Не оборачиваем err: url.Error содержит URL с токеном бота.
		return errors.New("telegram: запрос не выполнен")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	var out struct {
		Description string `json:"description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
	if out.Description != "" {
		return fmt.Errorf("telegram: HTTP %d: %s", resp.StatusCode, out.Description)
	}
	return fmt.Errorf("telegram: HTTP %d", resp.StatusCode)
}
