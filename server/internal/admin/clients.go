package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"fleetplatform/server/internal/store"
)

// LiveConns closes a client's live connection. The app owns connections, so
// the admin API reaches them only through this hook (implemented by
// *app.App). Nil means there is no running app (tests of the admin API alone):
// revocation still takes effect at the client's next hello.
type LiveConns interface {
	DisconnectClient(clientID string) bool
}

// Client states as reported by the admin API.
const (
	ClientActive  = "active"
	ClientRevoked = "revoked"
)

// ClientInfo describes one enrolled client. Tokens are never listed: only
// their hash is stored.
type ClientInfo struct {
	ID        string     `json:"id"`
	FleetID   string     `json:"fleet_id"`
	Kind      string     `json:"kind"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	State     string     `json:"state"`
}

type ClientList struct {
	Clients []ClientInfo `json:"clients"`
}

// RevokeClientResponse is the revoked client's record, plus whether a live
// connection was closed by this call.
type RevokeClientResponse struct {
	ClientInfo
	Disconnected bool `json:"disconnected"`
}

func clientInfo(c store.Client) ClientInfo {
	info := ClientInfo{
		ID: c.ID, FleetID: c.FleetID, Kind: string(c.Kind), Name: c.Name,
		CreatedAt: c.CreatedAt.UTC(), State: ClientActive,
	}
	if c.Revoked() {
		t := c.RevokedAt.UTC()
		info.RevokedAt = &t
		info.State = ClientRevoked
	}
	return info
}

func listClients(st store.Store, w http.ResponseWriter, r *http.Request) {
	fleet, ok := fleetFromPath(st, w, r)
	if !ok {
		return
	}
	clients, err := st.ListClients(fleet.ID)
	if err != nil {
		slog.Error("admin: list clients failed", "fleet", fleet.Name, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
		return
	}
	out := ClientList{Clients: make([]ClientInfo, 0, len(clients))}
	for _, c := range clients {
		out.Clients = append(out.Clients, clientInfo(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func revokeClient(st store.Store, live LiveConns, w http.ResponseWriter, r *http.Request) {
	fleet, ok := fleetFromPath(st, w, r)
	if !ok {
		return
	}
	c, err := st.RevokeClient(fleet.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrClientNotFound) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no such client in this fleet"})
		return
	}
	if err != nil {
		slog.Error("admin: revoke client failed", "fleet", fleet.Name, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
		return
	}
	// The token is dead in the store first, so a reconnect racing this call
	// is refused at hello; then the live connection, if any, is closed.
	disconnected := false
	if live != nil {
		disconnected = live.DisconnectClient(c.ID)
	}
	slog.Info("admin: revoked client", "fleet", fleet.Name, "client_id", c.ID, "kind", c.Kind, "disconnected", disconnected)
	writeJSON(w, http.StatusOK, RevokeClientResponse{ClientInfo: clientInfo(c), Disconnected: disconnected})
}
