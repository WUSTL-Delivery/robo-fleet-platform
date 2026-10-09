package signaling

import (
	"reflect"
	"testing"
	"time"

	"fleetplatform/sdk/go/protocol"
)

// turnVectorCredential is the output of
//
//	printf '1755103600:o_9c8d7e6f' | openssl dgst -sha1 -hmac 'north-star-turn-secret' -binary | base64
//
// so the scheme cannot drift from what a TURN server recomputes. The same
// username and credential are in protocol/fixtures/valid/ice-config.json.
const turnVectorCredential = "H5hhCnIJ5Q0i88m4zVts9A7C8Sc="

func TestTURNCredentialVector(t *testing.T) {
	username, credential := TURNCredential("north-star-turn-secret", time.Unix(1755103600, 0), "o_9c8d7e6f")
	if username != "1755103600:o_9c8d7e6f" || credential != turnVectorCredential {
		t.Fatalf("got %q %q", username, credential)
	}
}

func TestICEServers(t *testing.T) {
	now := time.Unix(1755100000, 123e6)

	// Nothing configured: an empty list, not nil, and no expiry.
	servers, expires := ICE{}.Servers(now, "r_1")
	if servers == nil || len(servers) != 0 || !expires.IsZero() {
		t.Fatalf("unconfigured: %#v %v", servers, expires)
	}

	// STUN alone carries no credentials and so no expiry.
	stun := []string{"stun:turn.example.org:3478"}
	servers, expires = ICE{STUNURLs: stun}.Servers(now, "r_1")
	if !reflect.DeepEqual(servers, []protocol.IceServer{{URLs: stun}}) || !expires.IsZero() {
		t.Fatalf("stun only: %#v %v", servers, expires)
	}

	turn := []string{"turn:turn.example.org:3478?transport=udp", "turns:turn.example.org:5349?transport=tcp"}
	ice := ICE{STUNURLs: stun, TURNURLs: turn, TURNSecret: "north-star-turn-secret", CredentialTTL: time.Hour}
	servers, expires = ice.Servers(now, "o_9c8d7e6f")
	if expires.Unix() != 1755103600 || expires.Nanosecond() != 0 {
		t.Fatalf("expires = %v", expires)
	}
	want := []protocol.IceServer{
		{URLs: stun},
		{URLs: turn, Username: "1755103600:o_9c8d7e6f", Credential: turnVectorCredential},
	}
	if !reflect.DeepEqual(servers, want) {
		t.Fatalf("servers = %#v", servers)
	}

	// The credential is bound to who asked and to when.
	other, _ := ice.Servers(now, "r_1a2b3c4d")
	later, _ := ice.Servers(now.Add(time.Second), "o_9c8d7e6f")
	if other[1].Credential == servers[1].Credential || later[1].Credential == servers[1].Credential {
		t.Fatal("credential does not depend on identity and expiry")
	}
}
