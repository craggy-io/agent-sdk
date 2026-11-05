module github.com/craggy-io/agent-sdk/cmd/craggy-agent

go 1.27.0

require (
	github.com/craggy-io/agent-sdk v0.0.0
	github.com/craggy-io/agent-sdk/adapters/configyaml v0.0.0-00010101000000-000000000000
	github.com/craggy-io/agent-sdk/adapters/tracesqlite v0.0.0
)

require (
	github.com/craggy-io/capability-services v0.1.1-0.20261105234106-63e308282f94 // indirect
	github.com/craggy-io/foundation-kit v0.0.0-20261105234626-e7c70ea2ccbc // indirect
	github.com/craggy-io/foundation-kit/adapters/sqlite v0.0.0-20261105234626-e7c70ea2ccbc // indirect
	github.com/craggy-io/script-services v0.1.1-0.20261105222027-e9623d91c30d // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	go.starlark.net v0.0.0-20261021141710-4f4267fb140d // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/sys v0.49.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.61.0 // indirect
)

replace github.com/craggy-io/agent-sdk => ../..

replace github.com/craggy-io/agent-sdk/adapters/configyaml => ../../adapters/configyaml

replace github.com/craggy-io/agent-sdk/adapters/tracesqlite => ../../adapters/tracesqlite
