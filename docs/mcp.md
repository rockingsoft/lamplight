# MCP server

Lamplight includes a Model Context Protocol server for agents that create,
inspect, validate, format, and run Lamplight tests and manage their project
configuration. It uses stdio, so the MCP
client starts it as a local subprocess:

```sh
lamplight mcp --working-dir /absolute/path/to/project
```

`--working-dir` uses the same upward `.lamplight` discovery as the other CLI
commands. `--config` selects an explicit configuration file. Use one or the
other to bind each MCP server entry to the intended project.

Example client configuration:

```json
{
  "mcpServers": {
    "lamplight": {
      "command": "/absolute/path/to/lamplight",
      "args": ["mcp", "--working-dir", "/absolute/path/to/project"]
    }
  }
}
```

## Tools

| Tool | Behavior |
| --- | --- |
| `lamplight_list_tests` | Lists tests, tags, source files, content hashes, datasource requirements, named targets, and the default target. |
| `lamplight_read_test_file` | Reads one `.wick` file below `project.base_dir`. |
| `lamplight_write_test_file` | Creates or replaces a complete `.wick` file, formats it, validates the project, and rolls back invalid changes. |
| `lamplight_delete_test_file` | Deletes a file and rolls back when the remaining project is invalid. |
| `lamplight_format_test_file` | Applies canonical HCL formatting to one file. |
| `lamplight_read_project_config` | Reads the active `.lamplight` file and returns its absolute path, content, and SHA-256. |
| `lamplight_write_project_config` | Replaces the active `.lamplight`, formats it, validates the entire project, and rolls back invalid changes. |
| `lamplight_format_project_config` | Applies canonical HCL formatting to the active `.lamplight` file. |
| `lamplight_lint_project` | Reports DSL diagnostics plus `.lamplight` and `.wick` files needing formatting without modifying anything. |
| `lamplight_run_tests` | Runs all tests, an exact test, or a tag against an optional named target and returns the JSON run result. |

Replacing, formatting, or deleting a file requires its current `sha256` from
`lamplight_read_test_file` or `lamplight_list_tests`. This prevents an agent
from silently overwriting changes made after it read the file. Creating a new
file requires an empty `expected_sha256` and never overwrites an existing one.

All file tools accept only relative `.wick` paths confined to the configured
`project.base_dir`. Absolute paths, traversal outside that directory, and
symbolic-link targets are rejected. Mutations are serialized and written by
atomic rename.

Project configuration writes use the same optimistic concurrency and rollback
rules. Read it first with `lamplight_read_project_config`, then pass the returned
hash to `lamplight_write_project_config` or
`lamplight_format_project_config`. Configuration content may include root
`variable` declarations, `project.default_target`, and `target` blocks for
`local`, `docker_compose`, and `kubernetes` runtimes.

`lamplight_run_tests.target` has the same semantics as CLI `--target`. When it
is omitted, Lamplight uses `project.default_target`; if the project has no
default target, execution is local.

Runtime variables can be passed to `lamplight_run_tests`, but secrets should
normally be supplied to the MCP subprocess as `LAMPLIGHT_VAR_<NAME>` environment
variables. Tool arguments can be retained in client logs or agent transcripts.
