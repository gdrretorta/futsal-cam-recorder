package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gdrretorta/futsal-cam-recorder/internal/config"
)

// HealthWatcher emails the admins when, during a recording, the camera has
// sent nothing for a while (and again when it is back), and when the
// recordings disk is running out of space. It only reads state: it never
// starts, stops or touches the recording.
type HealthWatcher struct {
	Config func() config.Config
	// Camera reports whether a recording should be running and, if so,
	// whether the camera is not delivering (connecting or reconnecting),
	// with the reason shown in the UI.
	Camera func() (recording, down bool, reason string)
	// Disk returns the free and total bytes of the recordings volume.
	Disk func() (free, total uint64, err error)
	Log  *slog.Logger
	Now  func() time.Time                                  // for tests
	Send func(context.Context, config.Notify, Email) error // for tests

	mu         sync.Mutex
	downSince  time.Time // first check that saw the camera down; zero while up
	downSent   bool      // the "camera down" alert went out for this outage
	downTry    time.Time // last failed attempt of the "camera down" alert
	diskSent   time.Time // last low-disk alert; zero while there is space
	diskTry    time.Time // last failed attempt of the low-disk alert
	lastReason string
}

// lowDiskRepeat is how often the low-disk alert is repeated while the space
// stays low.
const lowDiskRepeat = 24 * time.Hour

// diskMargin must be freed above the limit before a new low-disk episode
// counts, so free space hovering around the limit doesn't send an email
// every time it crosses it.
const diskMargin = 1 << 30

// Run checks every minute until ctx is done.
func (w *HealthWatcher) Run(ctx context.Context) {
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

// Check sends the alerts that are due.
func (w *HealthWatcher) Check(ctx context.Context) {
	now, send := time.Now, Send
	if w.Now != nil {
		now = w.Now
	}
	if w.Send != nil {
		send = w.Send
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	cfg := w.Config()
	w.checkCamera(ctx, cfg, now(), send)
	w.checkDisk(ctx, cfg, now(), send)
}

func (w *HealthWatcher) checkCamera(ctx context.Context, cfg config.Config, now time.Time, send func(context.Context, config.Notify, Email) error) {
	recording, down, reason := w.Camera()
	if !recording || !down {
		// A recording stopped during an outage isn't news; only a camera
		// that comes back while recording is.
		if recording && w.downSent && cfg.Notify.Enabled() {
			// Best effort: one attempt, the recording is fine again anyway.
			e := CameraBackEmail(cfg, w.downSince, now)
			if len(e.To) > 0 {
				if err := send(ctx, cfg.Notify, e); err != nil {
					w.Log.Warn("camera back alert not sent", "err", err)
				} else {
					w.Log.Info("camera back alert sent", "to", strings.Join(e.To, ", "))
				}
			}
		}
		w.downSince, w.downSent, w.downTry = time.Time{}, false, time.Time{}
		return
	}
	if w.downSince.IsZero() {
		w.downSince = now
	}
	if reason != "" {
		w.lastReason = reason
	}
	limit := time.Duration(cfg.Notify.CameraDownMinutes) * time.Minute
	if w.downSent || limit <= 0 || now.Sub(w.downSince) < limit || !cfg.Notify.Enabled() {
		return
	}
	if !w.downTry.IsZero() && now.Sub(w.downTry) < retryAfter {
		return
	}
	e := CameraDownEmail(cfg, w.downSince, now, w.lastReason)
	if len(e.To) == 0 {
		return
	}
	if err := send(ctx, cfg.Notify, e); err != nil {
		w.downTry = now
		w.Log.Warn("camera down alert not sent", "err", err)
		return
	}
	w.downSent, w.downTry = true, time.Time{}
	w.Log.Info("camera down alert sent", "to", strings.Join(e.To, ", "))
}

func (w *HealthWatcher) checkDisk(ctx context.Context, cfg config.Config, now time.Time, send func(context.Context, config.Notify, Email) error) {
	limit := uint64(cfg.Notify.LowDiskGB) << 30
	if limit == 0 {
		w.diskSent, w.diskTry = time.Time{}, time.Time{}
		return
	}
	free, total, err := w.Disk()
	if err != nil {
		return
	}
	if free >= limit {
		if free >= limit+diskMargin {
			w.diskSent = time.Time{}
		}
		return
	}
	if !w.diskSent.IsZero() && now.Sub(w.diskSent) < lowDiskRepeat {
		return
	}
	if !cfg.Notify.Enabled() || (!w.diskTry.IsZero() && now.Sub(w.diskTry) < retryAfter) {
		return
	}
	e := LowDiskEmail(cfg, free, total)
	if len(e.To) == 0 {
		return
	}
	if err := send(ctx, cfg.Notify, e); err != nil {
		w.diskTry = now
		w.Log.Warn("low disk alert not sent", "err", err)
		return
	}
	w.diskSent, w.diskTry = now, time.Time{}
	w.Log.Info("low disk alert sent", "freeGB", free>>30, "to", strings.Join(e.To, ", "))
}

// CameraDownEmail warns that the camera has sent nothing since since.
func CameraDownEmail(cfg config.Config, since, now time.Time, reason string) Email {
	text := fmt.Sprintf("Há uma gravação a decorrer, mas a câmara não envia imagem desde as %s (há %s). O gravador continua a tentar ligar sozinho.",
		since.Format("15:04"), minutes(now.Sub(since)))
	if reason != "" {
		text += "\n\nÚltimo erro: " + reason
	}
	text += "\n\nVerifique se a câmara e o DVR estão ligados e na rede. Recebe outro email quando a imagem voltar." + siteLine(cfg)
	return Email{
		To:      AdminEmails(cfg),
		Subject: cfg.Title + ": câmara sem imagem",
		Text:    text,
	}
}

// CameraBackEmail says the camera is sending images again.
func CameraBackEmail(cfg config.Config, since, now time.Time) Email {
	return Email{
		To:      AdminEmails(cfg),
		Subject: cfg.Title + ": a câmara voltou",
		Text: fmt.Sprintf("A câmara voltou a enviar imagem às %s, depois de %s sem imagem. A gravação continua normalmente.",
			now.Format("15:04"), minutes(now.Sub(since))) + siteLine(cfg),
	}
}

// LowDiskEmail warns that the recordings disk is almost full.
func LowDiskEmail(cfg config.Config, free, total uint64) Email {
	keep := "Apague gravações antigas na página do gravador, ou ative a limpeza automática nas definições."
	if cfg.RetentionDays > 0 {
		keep = fmt.Sprintf("As gravações com mais de %d dias já são apagadas automaticamente. Apague gravações na página do gravador, ou reduza os dias nas definições.", cfg.RetentionDays)
	}
	return Email{
		To:      AdminEmails(cfg),
		Subject: cfg.Title + ": pouco espaço em disco",
		Text: fmt.Sprintf("O disco das gravações tem só %s livres (de %s). Quando encher, as gravações param.\n\n%s\n\nEnquanto o espaço continuar abaixo de %d GB, recebe este aviso uma vez por dia.",
			gb(free), gb(total), keep, cfg.Notify.LowDiskGB) + siteLine(cfg),
	}
}

func minutes(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m == 1 {
		return "1 minuto"
	}
	return fmt.Sprintf("%d minutos", m)
}

func gb(b uint64) string {
	return strings.Replace(fmt.Sprintf("%.1f GB", float64(b)/(1<<30)), ".", ",", 1)
}
