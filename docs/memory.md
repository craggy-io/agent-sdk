# memory

Namespaced JSON values that outlive a run. `Store` is
`Put`/`Get`/`List`/`Delete` over `(namespace, key)`; `MemStore` for
tests and ephemeral hosts, `OpenFileStore(dir)` for one JSON document
per namespace with atomic rewrites. Values must be valid JSON;
namespaces and keys reject path separators.

On its own this package is just storage. Register it through
`agentcap` (`Options.Memory`) and `memory.put/get/list/delete`
become capabilities — callable from Starlark
(`memory.put(namespace = "demo", key = "k", value = {...})`), from
agents as tools (`memory_put`), and over MCP/HTTP, with the view and
definition filters deciding who may.
