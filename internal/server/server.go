package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/r59q/gitsnow/internal/issues"
)

type API struct {
	store  *issues.Store
	logger *slog.Logger
}

func New(store *issues.Store, assets fs.FS, logger *slog.Logger) http.Handler {
	api := &API{store: store, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/repository", func(w http.ResponseWriter, r *http.Request) {
		status, err := store.SyncStatus(r.Context())
		if err != nil {
			api.handleError(w, err)
			return
		}
		api.respond(w, http.StatusOK, map[string]any{
			"path": store.Root(), "metadataRef": issues.MetadataRef, "backend": "git", "sync": status,
		})
	})
	mux.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		status, err := store.SyncStatus(r.Context())
		if err != nil {
			api.handleError(w, err)
			return
		}
		store.RequestSync()
		if status.Remote != "" && status.State != "syncing" {
			status.State, status.Message = "pending", "Synchronization requested; local changes are safe"
		}
		api.respond(w, http.StatusAccepted, status)
	})
	mux.HandleFunc("GET /api/issues", api.list)
	mux.HandleFunc("POST /api/issues", api.create)
	mux.HandleFunc("GET /api/issues/{id}", api.get)
	mux.HandleFunc("PUT /api/issues/{id}", api.update)
	mux.HandleFunc("DELETE /api/issues/{id}", api.delete)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		api.failure(w, http.StatusNotFound, "not_found", "API endpoint not found")
	})
	mux.Handle("/", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if !LoopbackHost(r.Host) || !sameOrigin(r) {
			api.failure(w, http.StatusForbidden, "forbidden", "Only same-origin, localhost requests are allowed")
			return
		}
		if expected := r.Header.Get("X-Gitsnow-Repository"); expected != "" && expected != store.Root() {
			api.failure(w, http.StatusConflict, "repository_changed", "The daemon is serving a different repository; close this draft and reload")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func LoopbackHost(host string) bool {
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		host = hostname
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && parsed.Scheme == scheme && parsed.Host == r.Host && parsed.Path == "" && parsed.User == nil
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.List(r.Context(), r.URL.Query().Get("status"), r.URL.Query().Get("q"))
	if err != nil {
		a.handleError(w, err)
		return
	}
	a.respond(w, http.StatusOK, map[string]any{"issues": list})
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	issue, err := a.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.handleError(w, err)
		return
	}
	a.respond(w, http.StatusOK, issue)
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	var input issues.Input
	if !a.decode(w, r, &input) {
		return
	}
	if input.Revision != "" {
		a.failure(w, http.StatusBadRequest, "invalid_input", "New issues must not specify a revision")
		return
	}
	issue, err := a.store.Create(r.Context(), input)
	if err != nil {
		a.handleError(w, err)
		return
	}
	w.Header().Set("Location", "/api/issues/"+issue.ID)
	a.respond(w, http.StatusCreated, issue)
}

func (a *API) update(w http.ResponseWriter, r *http.Request) {
	var input issues.Input
	if !a.decode(w, r, &input) {
		return
	}
	issue, err := a.store.Update(r.Context(), r.PathValue("id"), input)
	if err != nil {
		a.handleError(w, err)
		return
	}
	a.respond(w, http.StatusOK, issue)
}

func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Revision string `json:"revision"`
	}
	if !a.decode(w, r, &input) {
		return
	}
	if err := a.store.Delete(r.Context(), r.PathValue("id"), input.Revision); err != nil {
		a.handleError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) decode(w http.ResponseWriter, r *http.Request, target any) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		a.failure(w, http.StatusUnsupportedMediaType, "invalid_content_type", "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*1024*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(target)
	if err == nil {
		var extra any
		if trailing := decoder.Decode(&extra); trailing != io.EOF {
			if trailing != nil {
				err = trailing
			} else {
				err = errors.New("request must contain exactly one JSON object")
			}
		}
	}
	if err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			a.failure(w, http.StatusRequestEntityTooLarge, "invalid_input", "Request must not exceed 2 MiB")
		} else {
			a.failure(w, http.StatusBadRequest, "invalid_input", "Request must be a single valid JSON object with known fields")
		}
		return false
	}
	return true
}

func (a *API) handleError(w http.ResponseWriter, err error) {
	var validation *issues.ValidationError
	switch {
	case errors.As(err, &validation):
		a.failure(w, http.StatusBadRequest, "invalid_input", validation.Error())
	case errors.Is(err, issues.ErrNotFound):
		a.failure(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, issues.ErrConflict):
		a.failure(w, http.StatusConflict, "conflict", err.Error())
	default:
		a.logger.Error("repository operation failed", "error", err)
		a.failure(w, http.StatusInternalServerError, "repository_error", "Repository operation failed; reload before retrying and check daemon logs")
	}
}

func (a *API) failure(w http.ResponseWriter, status int, code, message string) {
	a.respond(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func (a *API) respond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		a.logger.Error("write API response", "error", err)
	}
}
