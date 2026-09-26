package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestClientRevokeListAndAuth(t *testing.T) {
	st, f := openEnrollKeyLifecycleStore(t)
	other, err := st.CreateFleet("elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	robotTok, robot, err := st.CreateToken(f.ID, KindRobot, "bot")
	if err != nil {
		t.Fatal(err)
	}
	opTok, op, err := st.CreateToken(f.ID, KindOperator, "laptop")
	if err != nil {
		t.Fatal(err)
	}

	clients, err := st.ListClients(f.ID)
	if err != nil || len(clients) != 2 || clients[0].ID != robot.ID || clients[1].ID != op.ID {
		t.Fatalf("ListClients = %+v, %v", clients, err)
	}
	if clients[0].Revoked() || clients[0].CreatedAt.IsZero() || clients[0].Kind != KindRobot || clients[0].Name != "bot" {
		t.Fatalf("listed robot = %+v", clients[0])
	}

	if _, err := st.RevokeClient(other.ID, robot.ID); !errors.Is(err, ErrClientNotFound) {
		t.Fatalf("revoke through the wrong fleet: %v, want ErrClientNotFound", err)
	}
	if _, err := st.RevokeClient(f.ID, "r_missing"); !errors.Is(err, ErrClientNotFound) {
		t.Fatalf("revoke unknown id: %v", err)
	}

	c, err := st.RevokeClient(f.ID, robot.ID)
	if err != nil || !c.Revoked() || c.ID != robot.ID {
		t.Fatalf("RevokeClient = %+v, %v", c, err)
	}
	again, err := st.RevokeClient(f.ID, robot.ID)
	if err != nil || !again.RevokedAt.Equal(c.RevokedAt) {
		t.Fatalf("second revoke = %+v, %v; want unchanged %v", again, err, c.RevokedAt)
	}
	if _, ok, err := st.AuthToken(robotTok); ok || err != nil {
		t.Fatalf("revoked token authenticates: ok=%v err=%v", ok, err)
	}
	if got, ok, err := st.AuthToken(opTok); !ok || err != nil || got.ID != op.ID {
		t.Fatalf("other client's token affected: %+v ok=%v err=%v", got, ok, err)
	}
	if robots, err := st.RobotsInFleet(f.ID); err != nil || len(robots) != 0 {
		t.Fatalf("RobotsInFleet still lists the revoked robot: %+v, %v", robots, err)
	}
	clients, _ = st.ListClients(f.ID)
	if !clients[0].Revoked() || clients[1].Revoked() {
		t.Fatalf("ListClients after revoke = %+v", clients)
	}
}

// A v0.1.0 database (no clients.revoked_at) opens, keeps its tokens valid, and
// supports client revocation after migration, across restarts.
func TestClientRevokeMigratesV010Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet-v010.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const keepTok, loseTok = "fp-tk-keep-v010", "fp-tk-lose-v010"
	for _, stmt := range []string{
		v010Schema,
		`INSERT INTO fleets (id, name) VALUES ('f_v010', 'club')`,
		`INSERT INTO clients (id, fleet_id, kind, name, token_hash) VALUES ('r_keep', 'f_v010', 'robot', 'keep', '` + hashSecret(keepTok) + `')`,
		`INSERT INTO clients (id, fleet_id, kind, name, token_hash) VALUES ('r_lose', 'f_v010', 'robot', 'lose', '` + hashSecret(loseTok) + `')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	for round := 0; round < 2; round++ {
		st, err := OpenSqlite(path)
		if err != nil {
			t.Fatalf("round %d: open v0.1.0 db: %v", round, err)
		}
		if _, ok, err := st.AuthToken(keepTok); !ok || err != nil {
			t.Fatalf("round %d: v0.1.0 token rejected: %v %v", round, ok, err)
		}
		if round == 0 {
			if _, ok, _ := st.AuthToken(loseTok); !ok {
				t.Fatal("v0.1.0 token rejected before any revoke")
			}
			if _, err := st.RevokeClient("f_v010", "r_lose"); err != nil {
				t.Fatal(err)
			}
		}
		if _, ok, _ := st.AuthToken(loseTok); ok {
			t.Fatalf("round %d: revoked token authenticates", round)
		}
		clients, err := st.ListClients("f_v010")
		if err != nil || len(clients) != 2 || clients[0].Revoked() == clients[1].Revoked() {
			t.Fatalf("round %d: ListClients = %+v, %v", round, clients, err)
		}
		st.Close()
	}
}
