package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"
)

const clientUsage = "client: usage: fleetctl client list|revoke --fleet <name> [...]"

// clientInfo mirrors admin.ClientInfo (plus the revoke call's "disconnected");
// fleetctl speaks only the HTTP contract.
type clientInfo struct {
	ID           string     `json:"id"`
	FleetID      string     `json:"fleet_id"`
	Kind         string     `json:"kind"`
	Name         string     `json:"name"`
	CreatedAt    time.Time  `json:"created_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	State        string     `json:"state"`
	Disconnected bool       `json:"disconnected,omitempty"`
}

func cmdClient(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{clientUsage}
	}
	switch args[0] {
	case "list", "ls":
		return cmdClientList(args[1:], stdout, stderr)
	case "revoke":
		return cmdClientRevoke(args[1:], stdout, stderr)
	default:
		return usageError{fmt.Sprintf("client: unknown subcommand %q\n%s", args[0], clientUsage)}
	}
}

func clientsPath(fleet string) string {
	return "/api/admin/fleets/" + url.PathEscape(fleet) + "/clients"
}

func cmdClientList(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("client list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fleet := fs.String("fleet", "", "fleet name")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError{fmt.Sprintf("client list: unexpected argument %q", pos[0])}
	}
	if *fleet == "" {
		return usageError{"client list: --fleet is required"}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	raw, err := adminCall(cfg, http.MethodGet, clientsPath(*fleet), nil, *fleet)
	if err != nil {
		return err
	}
	var list struct {
		Clients []clientInfo `json:"clients"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("unexpected response from server: %s", strings.TrimSpace(string(raw)))
	}
	if len(list.Clients) == 0 {
		fmt.Fprintf(stdout, "fleet %q has no clients\n", *fleet)
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tNAME\tSTATE\tCREATED\tREVOKED")
	for _, c := range list.Clients {
		name := c.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Kind, name, c.State, fmtTime(&c.CreatedAt, "-"), fmtTime(c.RevokedAt, "-"))
	}
	return tw.Flush()
}

func cmdClientRevoke(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("client revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fleet := fs.String("fleet", "", "fleet name")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if *fleet == "" || len(pos) != 1 {
		return usageError{"client revoke: usage: fleetctl client revoke --fleet <name> <client id>  (ids from `fleetctl client list`)"}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	raw, err := adminCall(cfg, http.MethodPost, clientsPath(*fleet)+"/"+url.PathEscape(pos[0])+"/revoke", nil, *fleet)
	if err != nil {
		return err
	}
	var c clientInfo
	if err := json.Unmarshal(raw, &c); err != nil || c.ID == "" {
		return fmt.Errorf("unexpected response from server: %s", strings.TrimSpace(string(raw)))
	}
	fmt.Fprintf(stdout, "revoked %s %s in fleet %q (at %s)\n", c.Kind, c.ID, *fleet, fmtTime(c.RevokedAt, "unknown time"))
	if c.Disconnected {
		fmt.Fprintln(stdout, "Its live connection was closed.")
	}
	fmt.Fprintln(stdout, "Its token is refused from now on; to bring it back, enroll it again as a new client.")
	return nil
}
