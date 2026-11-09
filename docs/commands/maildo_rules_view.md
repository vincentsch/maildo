# maildo rules view

Show one Inbox rule

## Usage

```
maildo rules view <rule-id>
```

## Examples

```
maildo rules view rule-id
maildo rules view rule-id --account work
maildo rules view rule-id --json
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

- rules list
- rules health
