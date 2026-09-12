// Package protocol mirrors protocol/ (the JSON Schemas, which are the source of
// truth). contract_test.go keeps these structs honest against the schemas and
// fixtures — change the schema first, then this file.
package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

const Version = 0

// Message types (see protocol/catalog.json).
const (
	TypeEnrollRequest  = "enroll.request"
	TypeEnrollResponse = "enroll.response"
	TypeHello          = "hello"
	TypeWelcome        = "welcome"
	TypeHeartbeat      = "heartbeat"
	TypeManifest       = "manifest"
	TypeTelemetry      = "telemetry"
	TypeHelpRequest    = "help.request"
	TypeTwist          = "twist"
	TypeSubscribe      = "subscribe"
	TypeSnapshot       = "snapshot"
	TypeEvent          = "event"
	TypeLeaseClaim     = "lease.claim"
	TypeLeaseGranted   = "lease.granted"
	TypeLeaseRenew     = "lease.renew"
	TypeLeaseRelease   = "lease.release"
	TypeLeaseRevoked   = "lease.revoked"
	TypeChannelPublish = "channel.publish"
	TypeChannelMessage = "channel.message"
	TypeSignal         = "signal"
	TypeLayerDeclare   = "layer.declare"
	TypeLayerUpdate    = "layer.update"
	TypeError          = "error"
)

// Robot intervention FSM states (platform vocabulary only).
const (
	StateAutonomous    = "AUTONOMOUS"
	StateHelpRequested = "HELP_REQUESTED"
	StateTeleop        = "TELEOP"
)

// Event names.
const (
	EventRobotOnline        = "robot.online"
	EventRobotOffline       = "robot.offline"
	EventRobotTelemetry     = "robot.telemetry"
	EventRobotHelpRequested = "robot.help_requested"
	EventRobotLeaseGranted  = "robot.lease_granted"
	EventRobotLeaseReleased = "robot.lease_released"
	EventRobotLeaseRevoked  = "robot.lease_revoked"
)

// Lease revocation reasons.
const (
	RevokeReleased     = "released"
	RevokeExpired      = "expired"
	RevokeStolen       = "stolen"
	RevokeOperatorLost = "operator_lost"
)

// Error codes.
const (
	ErrAuthFailed     = "auth_failed"
	ErrInvalidMessage = "invalid_message"
	ErrNotFound       = "not_found"
	ErrNotAuthorized  = "not_authorized"
	ErrConflict       = "conflict"
	ErrRateLimited    = "rate_limited"
)

type Envelope struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	TsMs    int64           `json:"ts_ms,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

// Msg builds an envelope around a payload, stamping the current time.
func Msg(typ string, payload any) Envelope {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("protocol: marshal %s payload: %v", typ, err))
	}
	return Envelope{V: Version, Type: typ, TsMs: time.Now().UnixMilli(), Payload: raw}
}

type AgentInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type EnrollRequest struct {
	EnrollmentKey string     `json:"enrollment_key"`
	Kind          string     `json:"kind"`
	Name          string     `json:"name,omitempty"`
	Agent         *AgentInfo `json:"agent,omitempty"`
}

type EnrollResponse struct {
	Token    string `json:"token"`
	ClientID string `json:"client_id"`
	FleetID  string `json:"fleet_id"`
}

type Hello struct {
	Token string     `json:"token"`
	Agent *AgentInfo `json:"agent,omitempty"`
}

type Welcome struct {
	ClientID            string `json:"client_id"`
	FleetID             string `json:"fleet_id"`
	Kind                string `json:"kind"`
	ServerTimeMs        int64  `json:"server_time_ms"`
	HeartbeatIntervalMs int    `json:"heartbeat_interval_ms"`
}

type Heartbeat struct{}

type Drive struct {
	Type      string  `json:"type"`
	MaxVMps   float64 `json:"max_v_mps"`
	MaxWRadps float64 `json:"max_w_radps"`
}

type Camera struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

type Manifest struct {
	Drive     *Drive    `json:"drive,omitempty"`
	Cameras   []Camera  `json:"cameras,omitempty"`
	Battery   *struct{} `json:"battery,omitempty"`
	Channels  []string  `json:"channels,omitempty"`
	Behaviors []string  `json:"behaviors,omitempty"`
}

// Pose is frame-relative: frame "geographic" uses lat/lon/alt_m, frame "local"
// uses frame_id/x_m/y_m/z_m. Never assume lat/lon (DESIGN.md D7).
type Pose struct {
	Frame   string   `json:"frame"`
	Lat     *float64 `json:"lat,omitempty"`
	Lon     *float64 `json:"lon,omitempty"`
	AltM    *float64 `json:"alt_m,omitempty"`
	FrameID string   `json:"frame_id,omitempty"`
	XM      *float64 `json:"x_m,omitempty"`
	YM      *float64 `json:"y_m,omitempty"`
	ZM      *float64 `json:"z_m,omitempty"`
	YawRad  *float64 `json:"yaw_rad,omitempty"`
}

type Velocity struct {
	VMps   *float64 `json:"v_mps,omitempty"`
	WRadps *float64 `json:"w_radps,omitempty"`
}

type Battery struct {
	Pct     *float64 `json:"pct,omitempty"`
	Voltage *float64 `json:"voltage,omitempty"`
}

type Telemetry struct {
	Pose     *Pose          `json:"pose,omitempty"`
	Velocity *Velocity      `json:"velocity,omitempty"`
	Battery  *Battery       `json:"battery,omitempty"`
	Health   map[string]any `json:"health,omitempty"`
}

type HelpRequest struct {
	Reason  string         `json:"reason"`
	Context map[string]any `json:"context,omitempty"`
}

type Twist struct {
	LeaseID string       `json:"lease_id"`
	Linear  TwistLinear  `json:"linear"`
	Angular TwistAngular `json:"angular"`
}

type TwistLinear struct {
	XMps float64  `json:"x_mps"`
	YMps *float64 `json:"y_mps,omitempty"`
}

type TwistAngular struct {
	ZRadps float64 `json:"z_radps"`
}

type Subscribe struct {
	Topics []string `json:"topics"`
}

type Snapshot struct {
	Robots []RobotSummary `json:"robots"`
}

type RobotSummary struct {
	RobotID  string    `json:"robot_id"`
	Name     string    `json:"name,omitempty"`
	Presence string    `json:"presence"`
	State    string    `json:"state"`
	Manifest *Manifest `json:"manifest,omitempty"`
	Lease    *Lease    `json:"lease,omitempty"`
}

type Event struct {
	Event   string          `json:"event"`
	RobotID string          `json:"robot_id,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type LeaseClaim struct {
	RobotID string `json:"robot_id"`
}

type Lease struct {
	LeaseID     string `json:"lease_id"`
	RobotID     string `json:"robot_id"`
	OperatorID  string `json:"operator_id"`
	ExpiresAtMs int64  `json:"expires_at_ms"`
}

type LeaseRenew struct {
	LeaseID string `json:"lease_id"`
}

type LeaseRelease struct {
	LeaseID    string `json:"lease_id"`
	Resolution string `json:"resolution"`
}

type LeaseRevoked struct {
	LeaseID string `json:"lease_id"`
	RobotID string `json:"robot_id"`
	Reason  string `json:"reason"`
}

type ChannelPublish struct {
	Channel   string          `json:"channel"`
	To        string          `json:"to,omitempty"`
	Broadcast bool            `json:"broadcast,omitempty"`
	Data      json.RawMessage `json:"data"`
}

type ChannelMessage struct {
	Channel string          `json:"channel"`
	From    string          `json:"from"`
	Data    json.RawMessage `json:"data"`
}

type Signal struct {
	To   string          `json:"to,omitempty"`
	From string          `json:"from,omitempty"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type LayerDeclare struct {
	LayerID string         `json:"layer_id"`
	Kind    string         `json:"kind"`
	Title   string         `json:"title,omitempty"`
	Style   map[string]any `json:"style,omitempty"`
}

type LayerUpdate struct {
	LayerID string          `json:"layer_id"`
	Data    json.RawMessage `json:"data"`
}

type ErrorMsg struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Ref     string `json:"ref,omitempty"`
}
