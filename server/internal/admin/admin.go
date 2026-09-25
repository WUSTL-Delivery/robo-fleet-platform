// Package admin is the installation-level HTTP API that fleetctl calls
// (DESIGN.md D14). It is authenticated by one admin bearer token from the
// bootstrap config (FLEET_ADMIN_TOKEN) and is not mounted at all when that
// token is unset.
//
//	POST /api/admin/fleets/{fleet}/operator-invites   {"ttl": "24h"} (body optional)
//	  -> 200 {"key": "fp-oi-...", "fleet_id": "f_...", "expires_at": "RFC3339"}
//
//	POST /api/admin/fleets/{fleet}/enroll-keys                {"ttl": "720h"} (body optional; no ttl = never expires)
//	  -> 200 {"key": "fp-ek-...", "id": "ek_...", "fleet_id": "f_...", "created_at": ..., "expires_at"?: ..., "state": "active"}
//	GET  /api/admin/fleets/{fleet}/enroll-keys
//	  -> 200 {"enroll_keys": [{"id", "fleet_id", "created_at", "expires_at"?, "revoked_at"?, "state"}]}
//	POST /api/admin/fleets/{fleet}/enroll-keys/{id}/revoke
//	  -> 200 the revoked key's record (idempotent); 404 if the id is not in that fleet
//
// Revoking an enrollment key stops new enrollments with it; clients already
// enrolled through it keep their tokens.
//
// {fleet} is the fleet's name, the same name used by -bootstrap and
// FLEET_BOOTSTRAP_FLEET.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"fleetplatform/server/internal/store"
)

const (
	// DefaultInviteTTL applies when the request names no ttl.
	DefaultInviteTTL = 24 * time.Hour
	// MaxInviteTTL bounds how long an unredeemed invite can sit around.
	MaxInviteTTL = 30 * 24 * time.Hour

	maxBodyBytes = 4 << 10
)

// InviteRequest is the optional body of the mint call. TTL is a Go duration
// string ("24h", "90m").
type InviteRequest struct {
	TTL string `json:"ttl,omitempty"`
}

type InviteResponse struct {
	Key       string    `json:"key"`
	FleetID   string    `json:"fleet_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type errorBody struct {
	Error string `json:"error"`
}

// Handler returns the admin API, or nil when token is empty (admin API off).
// Every route requires "Authorization: Bearer <token>", compared in constant time.
func Handler(st store.Store, token string) http.Handler {
	if token == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/admin/fleets/{fleet}/operator-invites", func(w http.ResponseWriter, r *http.Request) {
		mintOperatorInvite(st, w, r)
	})
	mux.HandleFunc("POST /api/admin/fleets/{fleet}/enroll-keys", func(w http.ResponseWriter, r *http.Request) {
		mintEnrollKey(st, w, r)
	})
	mux.HandleFunc("GET /api/admin/fleets/{fleet}/enroll-keys", func(w http.ResponseWriter, r *http.Request) {
		listEnrollKeys(st, w, r)
	})
	mux.HandleFunc("POST /api/admin/fleets/{fleet}/enroll-keys/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		revokeEnrollKey(st, w, r)
	})
	return requireToken(token, mux)
}

func requireToken(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="fleet-admin"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "invalid or missing admin token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func mintOperatorInvite(st store.Store, w http.ResponseWriter, r *http.Request) {
	var req InviteRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "malformed request body"})
		return
	}
	ttl := DefaultInviteTTL
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil || d <= 0 || d > MaxInviteTTL {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "ttl must be a positive duration up to 720h"})
			return
		}
		ttl = d
	}

	fleet, ok := fleetFromPath(st, w, r)
	if !ok {
		return
	}

	key, expiresAt, err := st.CreateOperatorInvite(fleet.ID, ttl)
	if err != nil {
		slog.Error("admin: mint operator invite failed", "fleet", fleet.Name, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
		return
	}
	slog.Info("admin: minted operator invite", "fleet", fleet.Name, "expires_at", expiresAt)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, InviteResponse{Key: key, FleetID: fleet.ID, ExpiresAt: expiresAt.UTC()})
}

// fleetFromPath resolves the {fleet} path segment, writing the 404 or 500
// itself when it returns false.
func fleetFromPath(st store.Store, w http.ResponseWriter, r *http.Request) (store.Fleet, bool) {
	fleet, ok, err := st.FleetByName(r.PathValue("fleet"))
	if err != nil {
		slog.Error("admin: fleet lookup failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
		return store.Fleet{}, false
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no such fleet"})
		return store.Fleet{}, false
	}
	return fleet, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
