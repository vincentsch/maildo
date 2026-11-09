# maildo outlook archive-policy check

Show which archive and retention settings maildo can see

## Usage

```
maildo outlook archive-policy check
```

## Examples

```
maildo outlook archive-policy check
maildo outlook archive-policy check --account work
maildo outlook archive-policy check --json
```

## Flags

- `--account` Account to use (default: the active account)

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Request details

Add `--include-meta` to the JSON output to get the result as `{data, meta}`, where `meta` holds details about the request.

## Related commands

- folders list
- rules health
