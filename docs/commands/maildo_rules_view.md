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

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- rules list
- rules health
