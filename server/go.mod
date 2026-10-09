// Module path is a placeholder until the project is named (DESIGN.md open decision).
module fleetplatform/server

go 1.26

require (
	fleetplatform/sdk/go v0.0.0
	github.com/coder/websocket v1.8.15
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.56.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// The wire types (sdk/go/protocol) live in the Go SDK module so services can import
// them; the server always builds against the copy in this repo.
replace fleetplatform/sdk/go => ../sdk/go
