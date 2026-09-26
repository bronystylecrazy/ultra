# ultra

The CLI for [ultrastack](https://github.com/bronystylecrazy/ultrastack), the
LLM-first product factory for Go built on the
[di](https://github.com/bronystylecrazy/di) kernel.

```
go install github.com/bronystylecrazy/ultra@latest
```

| | |
|---|---|
| `ultra new` / `add` / `init` | scaffold a product, add a preset to one, adopt an existing module |
| `ultra dev` | the watch–rebuild–restart loop, API and frontend together |
| `ultra explain` / `codes` | the worked fix behind any diagnostic code |
| `ultra diff` / `breaking` | graph and contract drift between two revisions |
| `ultra vet` | the static analyzer (ultravet) over a product |
| `ultra fleet` / `upgrade` | sweep, verify and bump every product in a tree |
| `ultra req` / `trace` / `records` / `compliance` | ISO 29110 traceability, derived from the graph |
| `ultra mcp` | graph intelligence as MCP tools for agents |
| `ultra skill install` | vendor the doctrine pinned to a product's framework version |

`ultra help <command>` has the rest.

## Versions

The CLI releases on its own train. It pins what it scaffolds:
`scaffoldVersion` (the framework) and `scaffoldKernelVersion` (di), both in
`new.go`, each held to the framework's own requirements by a drift test.

## Developing

Tests that read or build against the framework use the pinned release from
the module cache by default. Point them at a checkout to prove a framework
change and a CLI change together:

```
ULTRASTACK_DIR=../ultrastack go test ./...
```

The docs drift gate (`ultra skill gen` over the framework's `references/`)
and the from-source ultravet test only run with a checkout.

`ultra skill gen` is run from the framework repo root, not from here.

## License

MIT
