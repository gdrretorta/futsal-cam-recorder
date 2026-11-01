// Package notify emails the admins through the Resend API
// (https://resend.com/docs/api-reference/emails/send-email), and watches for
// recordings that run too long.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/joaoopereira/simple-rtsp-recorder/internal/config"
)

// ResendURL is the endpoint emails are posted to (a variable for tests).
var ResendURL = "https://api.resend.com/emails"

var client = &http.Client{Timeout: 30 * time.Second}

// Email is one message.
type Email struct {
	To      []string
	Subject string
	Text    string
}

// ErrNotConfigured means there is no API key or sender yet.
var ErrNotConfigured = errors.New("os emails não estão configurados: indique a API key da Resend e o remetente")

// Send sends an email with the settings in n.
func Send(ctx context.Context, n config.Notify, e Email) error {
	if !n.Enabled() {
		return ErrNotConfigured
	}
	if len(e.To) == 0 {
		return errors.New("não há destinatários")
	}
	body, _ := json.Marshal(map[string]any{
		"from":    n.From,
		"to":      e.To,
		"subject": e.Subject,
		"text":    e.Text,
		"html":    toHTML(e.Text),
	})
	req, err := http.NewRequestWithContext(ctx, "POST", ResendURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.ResendAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("não foi possível contactar a Resend: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	// Resend explains the problem (bad key, unverified domain…) in "message".
	var r struct {
		Message string `json:"message"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(data, &r); r.Message == "" {
		r.Message = resp.Status
	}
	return fmt.Errorf("a Resend recusou o email (%d): %s", resp.StatusCode, r.Message)
}

// toHTML keeps the text as is, with links clickable.
func toHTML(text string) string {
	var b strings.Builder
	b.WriteString(`<div style="font-family:system-ui,sans-serif;font-size:15px;line-height:1.5">`)
	for _, line := range strings.Split(text, "\n") {
		esc := html.EscapeString(line)
		for _, word := range strings.Fields(line) {
			if strings.HasPrefix(word, "https://") || strings.HasPrefix(word, "http://") {
				w := html.EscapeString(word)
				esc = strings.Replace(esc, w, `<a href="`+w+`">`+w+`</a>`, 1)
			}
		}
		b.WriteString(esc + "<br>\n")
	}
	b.WriteString("</div>")
	return b.String()
}

// AdminEmails lists the admins' addresses, the recipients of every alert.
func AdminEmails(cfg config.Config) []string {
	var to []string
	for _, u := range cfg.Users {
		if u.Role == config.RoleAdmin {
			to = append(to, u.Email)
		}
	}
	return to
}

// TestEmail is sent by the "send test email" button.
func TestEmail(cfg config.Config) Email {
	return Email{
		To:      AdminEmails(cfg),
		Subject: cfg.Title + ": email de teste",
		Text:    "Os emails do gravador estão a funcionar. É aqui que vai receber os avisos de gravações longas." + siteLine(cfg),
	}
}

func siteLine(cfg config.Config) string {
	if cfg.Notify.SiteURL == "" {
		return ""
	}
	return "\n\n" + cfg.Notify.SiteURL
}

// LongRecordingEmail warns that a recording started at startedAt has been
// running for hours.
func LongRecordingEmail(cfg config.Config, startedAt time.Time, hours int) Email {
	stop := "Se já não for precisa, pare-a na página do gravador."
	if cfg.Notify.SiteURL != "" {
		stop = "Se já não for precisa, pare-a em " + cfg.Notify.SiteURL
	}
	return Email{
		To:      AdminEmails(cfg),
		Subject: fmt.Sprintf("%s: gravação a decorrer há %d horas", cfg.Title, hours),
		Text: fmt.Sprintf("A gravação começou a %s às %s e continua a decorrer há %d horas.\n\n%s\n\nEnquanto a gravação continuar, recebe um novo aviso a cada hora.",
			startedAt.Format("02/01/2006"), startedAt.Format("15:04"), hours, stop),
	}
}

// Watcher sends the long-recording alert: once the recording reaches the
// configured number of hours, and again every hour after that.
type Watcher struct {
	Config    func() config.Config
	StartedAt func() *time.Time // nil while not recording
	Log       *slog.Logger
	Now       func() time.Time                                  // for tests
	Send      func(context.Context, config.Notify, Email) error // for tests

	mu        sync.Mutex
	session   time.Time // startedAt of the recording being watched
	sentHours int       // last alert sent for it, in hours
	lastTry   time.Time // last failed attempt, to retry calmly
}

// retryAfter spaces out attempts after a failure (network down, bad key).
const retryAfter = 10 * time.Minute

// Run checks every minute until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		w.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check sends the alert if it is due.
func (w *Watcher) Check(ctx context.Context) {
	now, send := time.Now, Send
	if w.Now != nil {
		now = w.Now
	}
	if w.Send != nil {
		send = w.Send
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	started := w.StartedAt()
	if started == nil {
		w.session = time.Time{}
		return
	}
	if !started.Equal(w.session) {
		w.session, w.sentHours, w.lastTry = *started, 0, time.Time{}
	}
	cfg := w.Config()
	limit := cfg.Notify.LongRecordingHours
	hours := int(now().Sub(*started) / time.Hour)
	if limit <= 0 || hours < limit || hours <= w.sentHours || !cfg.Notify.Enabled() {
		return
	}
	if !w.lastTry.IsZero() && now().Sub(w.lastTry) < retryAfter {
		return
	}
	email := LongRecordingEmail(cfg, *started, hours)
	if len(email.To) == 0 {
		return
	}
	if err := send(ctx, cfg.Notify, email); err != nil {
		w.lastTry = now()
		w.Log.Warn("long recording alert not sent", "hours", hours, "err", err)
		return
	}
	w.sentHours, w.lastTry = hours, time.Time{}
	w.Log.Info("long recording alert sent", "hours", hours, "to", strings.Join(email.To, ", "))
}
