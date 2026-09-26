package admin

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"fleetplatform/server/internal/store"
)

// Enrollment key states as reported by the admin API.
const (
	EnrollKeyActive  = "active"
	EnrollKeyExpired = "expired"
	EnrollKeyRevoked = "revoked"
)

// EnrollKeyRequest is the optional body of the enrollment key mint call. TTL
// is a Go duration string ("720h"); empty means the key never expires.
type EnrollKeyRequest struct {
	TTL string `json:"ttl,omitempty"`
}

// EnrollKeyInfo describes one enrollment key. The plaintext key is only ever
// present in the mint response (Key); it is never stored or listed.
type EnrollKeyInfo struct {
	Key       string     `json:"key,omitempty"`
	ID        string     `json:"id"`
	FleetID   string     `json:"fleet_id"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	State     string     `json:"state"`
}

type EnrollKeyList struct {
	EnrollKeys []EnrollKeyInfo `json:"enroll_keys"`
}

func enrollKeyInfo(k store.EnrollKey, now time.Time) EnrollKeyInfo {
	info := EnrollKeyInfo{ID: k.ID, FleetID: k.FleetID, CreatedAt: k.CreatedAt.UTC(), State: EnrollKeyActive}
	if !k.ExpiresAt.IsZero() {
		t := k.ExpiresAt.UTC()
		info.ExpiresAt = &t
	}
	if k.Revoked() {
		t := k.RevokedAt.UTC()
		info.RevokedAt = &t
	}
	switch {
	case k.Revoked():
		info.State = EnrollKeyRevoked
	case k.Expired(now):
		info.State = EnrollKeyExpired
	}
	return info
}

func mintEnrollKey(st store.Store, w http.ResponseWriter, r *http.Request) {
	var req EnrollKeyRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "malformed request body"})
		return
	}
	var ttl time.Duration
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil || d <= 0 {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "ttl must be a positive duration, or omitted for a key that never expires"})
			return
		}
		ttl = d
	}
	fleet, ok := fleetFromPath(st, w, r)
	if !ok {
		return
	}
	key, k, err := st.MintEnrollKey(fleet.ID, ttl)
	if err != nil {
		slog.Error("admin: mint enrollment key failed", "fleet", fleet.Name, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
		return
	}
	expires := "never"
	if !k.ExpiresAt.IsZero() {
		expires = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	slog.Info("admin: minted enrollment key", "fleet", fleet.Name, "key_id", k.ID, "expires_at", expires)
	info := enrollKeyInfo(k, time.Now())
	info.Key = key
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, info)
}

func listEnrollKeys(st store.Store, w http.ResponseWriter, r *http.Request) {
	fleet, ok := fleetFromPath(st, w, r)
	if !ok {
		return
	}
	keys, err := st.ListEnrollKeys(fleet.ID)
	if err != nil {
		slog.Error("admin: list enrollment keys failed", "fleet", fleet.Name, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
		return
	}
	now := time.Now()
	out := EnrollKeyList{EnrollKeys: make([]EnrollKeyInfo, 0, len(keys))}
	for _, k := range keys {
		out.EnrollKeys = append(out.EnrollKeys, enrollKeyInfo(k, now))
	}
	writeJSON(w, http.StatusOK, out)
}

func revokeEnrollKey(st store.Store, w http.ResponseWriter, r *http.Request) {
	fleet, ok := fleetFromPath(st, w, r)
	if !ok {
		return
	}
	k, err := st.RevokeEnrollKey(fleet.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrEnrollKeyNotFound) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no such enrollment key in this fleet"})
		return
	}
	if err != nil {
		slog.Error("admin: revoke enrollment key failed", "fleet", fleet.Name, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
		return
	}
	slog.Info("admin: revoked enrollment key", "fleet", fleet.Name, "key_id", k.ID)
	writeJSON(w, http.StatusOK, enrollKeyInfo(k, time.Now()))
}
