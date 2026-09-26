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

const enrollKeyUsage = "enroll-key: usage: fleetctl enroll-key create|list|revoke --fleet <name> [...]"

// enrollKeyInfo mirrors admin.EnrollKeyInfo; fleetctl stays independent of
// server packages and speaks only the HTTP contract.
type enrollKeyInfo struct {
	Key       string     `json:"key,omitempty"`
	ID        string     `json:"id"`
	FleetID   string     `json:"fleet_id"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	State     string     `json:"state"`
}

func cmdEnrollKey(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{enrollKeyUsage}
	}
	switch args[0] {
	case "create":
		return cmdEnrollKeyCreate(args[1:], stdout, stderr)
	case "list", "ls":
		return cmdEnrollKeyList(args[1:], stdout, stderr)
	case "revoke":
		return cmdEnrollKeyRevoke(args[1:], stdout, stderr)
	default:
		return usageError{fmt.Sprintf("enroll-key: unknown subcommand %q\n%s", args[0], enrollKeyUsage)}
	}
}

// parseInterspersed parses flags that may come before or after positional
// arguments ("revoke ek_1 --fleet club" and "revoke --fleet club ek_1").
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func enrollKeysPath(fleet string) string {
	return "/api/admin/fleets/" + url.PathEscape(fleet) + "/enroll-keys"
}

func cmdEnrollKeyCreate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("enroll-key create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fleet := fs.String("fleet", "", "fleet name (as created with -bootstrap / FLEET_BOOTSTRAP_FLEET)")
	ttl := fs.Duration("ttl", 0, "how long the key can enroll new clients (default: never expires)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError{fmt.Sprintf("enroll-key create: unexpected argument %q", pos[0])}
	}
	if *fleet == "" {
		return usageError{"enroll-key create: --fleet is required"}
	}
	if *ttl < 0 {
		return usageError{"enroll-key create: --ttl must be positive"}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var body any
	if *ttl > 0 {
		body = map[string]string{"ttl": ttl.String()}
	}
	raw, err := adminCall(cfg, http.MethodPost, enrollKeysPath(*fleet), body, *fleet)
	if err != nil {
		return err
	}
	var k enrollKeyInfo
	if err := json.Unmarshal(raw, &k); err != nil || k.Key == "" {
		return fmt.Errorf("unexpected response from server: %s", strings.TrimSpace(string(raw)))
	}
	expiry := "never expires"
	if k.ExpiresAt != nil {
		expiry = "expires " + k.ExpiresAt.Local().Format(time.RFC1123)
	}
	fmt.Fprintf(stdout, "Enrollment key %s for fleet %q (%s):\n\n", k.ID, *fleet, expiry)
	fmt.Fprintf(stdout, "    %s\n\n", k.Key)
	fmt.Fprintf(stdout, "Robots and services send it once as enrollment_key; it is not shown again.\n")
	fmt.Fprintf(stdout, "Revoke it with: fleetctl enroll-key revoke --fleet %s %s\n", *fleet, k.ID)
	return nil
}

func cmdEnrollKeyList(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("enroll-key list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fleet := fs.String("fleet", "", "fleet name")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError{fmt.Sprintf("enroll-key list: unexpected argument %q", pos[0])}
	}
	if *fleet == "" {
		return usageError{"enroll-key list: --fleet is required"}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	raw, err := adminCall(cfg, http.MethodGet, enrollKeysPath(*fleet), nil, *fleet)
	if err != nil {
		return err
	}
	var list struct {
		EnrollKeys []enrollKeyInfo `json:"enroll_keys"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("unexpected response from server: %s", strings.TrimSpace(string(raw)))
	}
	if len(list.EnrollKeys) == 0 {
		fmt.Fprintf(stdout, "fleet %q has no enrollment keys\n", *fleet)
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tCREATED\tEXPIRES\tREVOKED")
	for _, k := range list.EnrollKeys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.ID, k.State, fmtTime(&k.CreatedAt, "-"), fmtTime(k.ExpiresAt, "never"), fmtTime(k.RevokedAt, "-"))
	}
	return tw.Flush()
}

func cmdEnrollKeyRevoke(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("enroll-key revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fleet := fs.String("fleet", "", "fleet name")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if *fleet == "" || len(pos) != 1 {
		return usageError{"enroll-key revoke: usage: fleetctl enroll-key revoke --fleet <name> <key id>  (ids from `fleetctl enroll-key list`)"}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	raw, err := adminCall(cfg, http.MethodPost, enrollKeysPath(*fleet)+"/"+url.PathEscape(pos[0])+"/revoke", nil, *fleet)
	if err != nil {
		return err
	}
	var k enrollKeyInfo
	if err := json.Unmarshal(raw, &k); err != nil || k.ID == "" {
		return fmt.Errorf("unexpected response from server: %s", strings.TrimSpace(string(raw)))
	}
	fmt.Fprintf(stdout, "revoked enrollment key %s in fleet %q (at %s)\n", k.ID, *fleet, fmtTime(k.RevokedAt, "unknown time"))
	fmt.Fprintln(stdout, "New enrollments with it are refused; clients already enrolled with it keep their tokens.")
	return nil
}

func fmtTime(t *time.Time, zero string) string {
	if t == nil || t.IsZero() {
		return zero
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}
