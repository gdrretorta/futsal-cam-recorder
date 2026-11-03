package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gdrretorta/futsal-cam-recorder/internal/recorder"
)

// Live view (HLS, see recorder/live.go). Nothing here may be cached, by the
// browser or by Cloudflare: the URLs have no file extension and every answer
// says no-store.

func (s *Server) livePlaylist(w http.ResponseWriter, r *http.Request) {
	viewer := r.URL.Query().Get("viewer")
	if viewer == "" || len(viewer) > 64 {
		writeError(w, http.StatusBadRequest, errors.New("pedido inválido"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	pl, err := s.Recorder.LivePlaylist(ctx, viewer)
	if err != nil {
		writeLiveError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(pl)
}

func (s *Server) liveInit(w http.ResponseWriter, r *http.Request) {
	gen, err := strconv.Atoi(r.PathValue("gen"))
	data, err2 := s.Recorder.LiveInit(gen)
	if err != nil || err2 != nil {
		writeLiveError(w, recorder.ErrLiveGone)
		return
	}
	writeLiveMP4(w, data)
}

func (s *Server) liveSegment(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.Atoi(r.PathValue("seq"))
	if err != nil {
		writeLiveError(w, recorder.ErrLiveGone)
		return
	}
	data, err := s.Recorder.LiveSegment(seq)
	if err != nil {
		writeLiveError(w, err)
		return
	}
	writeLiveMP4(w, data)
}

func writeLiveMP4(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func writeLiveError(w http.ResponseWriter, err error) {
	w.Header().Set("Cache-Control", "no-store")
	code := http.StatusBadGateway // the camera didn't answer
	switch {
	case errors.Is(err, recorder.ErrLiveGone):
		code = http.StatusNotFound
	case errors.Is(err, recorder.ErrLiveFull):
		code = http.StatusTooManyRequests
	case errors.Is(err, recorder.ErrLiveUnavailable), errors.Is(err, context.DeadlineExceeded):
		code, err = http.StatusServiceUnavailable, recorder.ErrLiveUnavailable
	case errors.Is(err, recorder.ErrNotConfigured):
		code = http.StatusConflict
	}
	writeError(w, code, err)
}
