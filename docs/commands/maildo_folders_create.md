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

This command changes things. Add `--dry-run` to see what it would change; nothing changes then.

## Request details

Add `--include-meta` to the JSON output to get the result as `{data, meta}`, where `meta` holds details about the request.

## Related commands

- folders list
- folders view
- folders rename
