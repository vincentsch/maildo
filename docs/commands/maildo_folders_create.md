# maildo folders create

Create a folder

## Usage

```
maildo folders create <name>
```

## Examples

```
maildo folders create Clients --account work
maildo folders create ACME --parent Clients
maildo folders create Receipts --dry-run --json
```

## Flags

- `--account` Account to use (default: the active account)
- `--parent` Create the folder inside this folder

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Preview changes

This command makes changes. Use `--dry-run` to see what would change without changing anything.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- folders list
- folders view
- folders rename
