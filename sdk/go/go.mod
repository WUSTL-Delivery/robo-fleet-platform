// Module path is a placeholder until the project is named (DESIGN.md open decision).
// It mirrors the directory (fleetplatform/<path in repo>), like fleetplatform/server,
// so the rename is one search-and-replace of the "fleetplatform" prefix.
module fleetplatform/sdk/go

// Oldest Go the SDK supports; services importing it may be on an older toolchain
// than the server.
go 1.24.0

require github.com/santhosh-tekuri/jsonschema/v6 v6.0.3

require golang.org/x/text v0.14.0 // indirect
