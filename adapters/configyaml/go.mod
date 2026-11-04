module github.com/craggy-io/agent-sdk/adapters/configyaml

go 1.27.0

require (
	github.com/craggy-io/agent-sdk v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/google/uuid v1.6.0 // indirect

replace github.com/craggy-io/agent-sdk => ../..
