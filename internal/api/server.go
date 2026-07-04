// Package api exposes the avdd HTTP control plane.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/avd-farm/avd-farm/internal/config"
	"github.com/avd-farm/avd-farm/internal/farm"
)

// Server routes HTTP requests to the device manager.
type Server struct {
	cfg *config.Config
	mgr *farm.Manager
}

func New(cfg *config.Config, mgr *farm.Manager) *Server {
	return &Server{cfg: cfg, mgr: mgr}
}

// Handler returns the routed http.Handler for the control plane.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /devices", s.startDevice)
	mux.HandleFunc("GET /devices", s.listDevices)
	mux.HandleFunc("GET /devices/{id}", s.getDevice)
	mux.HandleFunc("DELETE /devices/{id}", s.deleteDevice)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// startResponse is the wire shape of a successful POST /devices.
type startResponse struct {
	ID     string `json:"id"`
	ADB    string `json:"adb"`
	Screen string `json:"screen"`
}

// deviceView is a device plus its client-facing data-plane endpoints.
type deviceView struct {
	*farm.Device
	ADB    string `json:"adb"`
	Screen string `json:"screen"`
}

func (s *Server) view(dev *farm.Device) deviceView {
	return deviceView{
		Device: dev,
		ADB:    fmt.Sprintf("%s:%d", s.cfg.PublishHost, dev.ADBPort),
		Screen: fmt.Sprintf("http://%s:%d/", s.cfg.PublishHost, dev.VNCPort),
	}
}

func (s *Server) startDevice(w http.ResponseWriter, r *http.Request) {
	var req farm.StartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.API <= 0 {
		writeError(w, http.StatusBadRequest, "\"api\" must be a positive Android API level")
		return
	}
	dev, err := s.mgr.Start(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, farm.ErrBadRequest):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, farm.ErrAtCapacity):
			writeError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, farm.ErrPortsExhausted):
			writeError(w, http.StatusServiceUnavailable, err.Error())
		case errors.Is(err, farm.ErrBootTimeout):
			writeError(w, http.StatusGatewayTimeout, err.Error())
		default:
			log.Printf("start device: %v", err)
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, startResponse{
		ID:     dev.ID,
		ADB:    fmt.Sprintf("%s:%d", s.cfg.PublishHost, dev.ADBPort),
		Screen: fmt.Sprintf("http://%s:%d/", s.cfg.PublishHost, dev.VNCPort),
	})
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	devs := s.mgr.List()
	views := make([]deviceView, 0, len(devs))
	for _, dev := range devs {
		views = append(views, s.view(dev))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getDevice(w http.ResponseWriter, r *http.Request) {
	dev, err := s.mgr.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(dev))
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Stop(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
