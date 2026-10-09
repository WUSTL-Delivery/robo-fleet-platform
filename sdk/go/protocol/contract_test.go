package protocol_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"fleetplatform/sdk/go/protocol"
)

const schemaBase = "https://fleetplatform.local/v0/"

func protocolDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "protocol"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

type catalog struct {
	Envelope string            `json:"envelope"`
	Messages map[string]string `json:"messages"`
}

type validators struct {
	envelope *jsonschema.Schema
	byType   map[string]*jsonschema.Schema
}

func loadValidators(t *testing.T) *validators {
	t.Helper()
	dir := protocolDir(t)

	c := jsonschema.NewCompiler()
	schemaFiles, err := filepath.Glob(filepath.Join(dir, "schemas", "*.schema.json"))
	if err != nil || len(schemaFiles) == 0 {
		t.Fatalf("no schemas found: %v", err)
	}
	for _, f := range schemaFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		id := schemaBase + filepath.Base(f)
		if err := c.AddResource(id, doc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}

	catData, err := os.ReadFile(filepath.Join(dir, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cat catalog
	if err := json.Unmarshal(catData, &cat); err != nil {
		t.Fatal(err)
	}

	v := &validators{byType: make(map[string]*jsonschema.Schema)}
	v.envelope, err = c.Compile(schemaBase + strings.TrimPrefix(cat.Envelope, "schemas/"))
	if err != nil {
		t.Fatalf("compile envelope: %v", err)
	}
	for typ, ref := range cat.Messages {
		sch, err := c.Compile(schemaBase + strings.TrimPrefix(ref, "schemas/"))
		if err != nil {
			t.Fatalf("compile %s (%s): %v", typ, ref, err)
		}
		v.byType[typ] = sch
	}
	return v
}

// check validates raw envelope bytes: envelope schema, known type, payload schema.
func (v *validators) check(raw []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := v.envelope.Validate(doc); err != nil {
		return err
	}
	m := doc.(map[string]any)
	typ, _ := m["type"].(string)
	sch, ok := v.byType[typ]
	if !ok {
		return &unknownType{typ}
	}
	return sch.Validate(m["payload"])
}

type unknownType struct{ typ string }

func (e *unknownType) Error() string { return "unknown message type: " + e.typ }

// payloadFactories maps every catalog type to its Go struct, so fixtures can be
// round-tripped: schema-valid JSON → struct → JSON → still schema-valid.
var payloadFactories = map[string]func() any{
	protocol.TypeEnrollRequest:  func() any { return &protocol.EnrollRequest{} },
	protocol.TypeEnrollResponse: func() any { return &protocol.EnrollResponse{} },
	protocol.TypeHello:          func() any { return &protocol.Hello{} },
	protocol.TypeWelcome:        func() any { return &protocol.Welcome{} },
	protocol.TypeHeartbeat:      func() any { return &protocol.Heartbeat{} },
	protocol.TypeManifest:       func() any { return &protocol.Manifest{} },
	protocol.TypeTelemetry:      func() any { return &protocol.Telemetry{} },
	protocol.TypeHelpRequest:    func() any { return &protocol.HelpRequest{} },
	protocol.TypeTwist:          func() any { return &protocol.Twist{} },
	protocol.TypeSubscribe:      func() any { return &protocol.Subscribe{} },
	protocol.TypeSnapshot:       func() any { return &protocol.Snapshot{} },
	protocol.TypeEvent:          func() any { return &protocol.Event{} },
	protocol.TypeLeaseClaim:     func() any { return &protocol.LeaseClaim{} },
	protocol.TypeLeaseGranted:   func() any { return &protocol.Lease{} },
	protocol.TypeLeaseRenew:     func() any { return &protocol.LeaseRenew{} },
	protocol.TypeLeaseRelease:   func() any { return &protocol.LeaseRelease{} },
	protocol.TypeLeaseRevoked:   func() any { return &protocol.LeaseRevoked{} },
	protocol.TypeWatch:          func() any { return &protocol.Watch{} },
	protocol.TypeChannelPublish: func() any { return &protocol.ChannelPublish{} },
	protocol.TypeChannelMessage: func() any { return &protocol.ChannelMessage{} },
	protocol.TypeSignal:         func() any { return &protocol.Signal{} },
	protocol.TypeIceRequest:     func() any { return &protocol.IceRequest{} },
	protocol.TypeIceConfig:      func() any { return &protocol.IceConfig{} },
	protocol.TypeLayerDeclare:   func() any { return &protocol.LayerDeclare{} },
	protocol.TypeLayerUpdate:    func() any { return &protocol.LayerUpdate{} },
	protocol.TypeError:          func() any { return &protocol.ErrorMsg{} },
}

func TestValidFixtures(t *testing.T) {
	v := loadValidators(t)
	files, err := filepath.Glob(filepath.Join(protocolDir(t), "fixtures", "valid", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no valid fixtures found: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.check(raw); err != nil {
				t.Fatalf("fixture should validate: %v", err)
			}

			// Round-trip through the Go structs.
			var env protocol.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			factory, ok := payloadFactories[env.Type]
			if !ok {
				t.Fatalf("no Go struct registered for type %q", env.Type)
			}
			payload := factory()
			if err := json.Unmarshal(env.Payload, payload); err != nil {
				t.Fatalf("unmarshal into struct: %v", err)
			}
			rt := protocol.Msg(env.Type, payload)
			rtRaw, err := json.Marshal(rt)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.check(rtRaw); err != nil {
				t.Fatalf("round-trip through Go struct lost schema validity: %v\nround-tripped: %s", err, rtRaw)
			}
		})
	}
}

func TestInvalidFixtures(t *testing.T) {
	v := loadValidators(t)
	files, err := filepath.Glob(filepath.Join(protocolDir(t), "fixtures", "invalid", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no invalid fixtures found: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.check(raw); err == nil {
				t.Fatal("fixture in fixtures/invalid/ unexpectedly validated")
			}
		})
	}
}

func TestCatalogCoversAllFixtureTypes(t *testing.T) {
	v := loadValidators(t)
	for typ := range payloadFactories {
		if _, ok := v.byType[typ]; !ok {
			t.Errorf("Go type constant %q missing from catalog.json", typ)
		}
	}
	for typ := range v.byType {
		if _, ok := payloadFactories[typ]; !ok {
			t.Errorf("catalog type %q has no Go struct registered", typ)
		}
	}
}
