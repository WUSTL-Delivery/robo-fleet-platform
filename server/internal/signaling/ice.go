package signaling

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"time"

	"fleetplatform/sdk/go/protocol"
)

// ICE is the installation's STUN and TURN servers, as clients are told about
// them. The server only hands out addresses and credentials: it never relays
// media and never talks to the TURN server (D2).
type ICE struct {
	STUNURLs []string
	TURNURLs []string
	// TURNSecret is the secret shared with the TURN server (coturn's
	// static-auth-secret). It signs credentials and is never sent to a client.
	TURNSecret string
	// CredentialTTL is how long a minted TURN credential is accepted.
	CredentialTTL time.Duration
}

// Servers returns the ICE servers for one client, with a TURN credential
// minted for it, and when that credential expires (the zero time when there
// is none). The slice is never nil: no configuration is an empty list.
func (c ICE) Servers(now time.Time, clientID string) ([]protocol.IceServer, time.Time) {
	servers := []protocol.IceServer{}
	if len(c.STUNURLs) > 0 {
		servers = append(servers, protocol.IceServer{URLs: c.STUNURLs})
	}
	if len(c.TURNURLs) == 0 {
		return servers, time.Time{}
	}
	// Whole seconds: the expiry is carried in the username as unix seconds.
	expires := now.Add(c.CredentialTTL).Truncate(time.Second)
	username, credential := TURNCredential(c.TURNSecret, expires, clientID)
	servers = append(servers, protocol.IceServer{URLs: c.TURNURLs, Username: username, Credential: credential})
	return servers, expires
}

// TURNCredential mints a time-limited TURN credential in the "TURN REST API"
// scheme that coturn implements as use-auth-secret: the username is the expiry
// in unix seconds, a colon, and an identity (only logged by the TURN server);
// the credential is base64(HMAC-SHA1(secret, username)). The TURN server
// recomputes the HMAC, so nothing is stored and nothing is revoked: a
// credential simply stops working at its expiry.
func TURNCredential(secret string, expires time.Time, identity string) (username, credential string) {
	username = strconv.FormatInt(expires.Unix(), 10) + ":" + identity
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return username, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
