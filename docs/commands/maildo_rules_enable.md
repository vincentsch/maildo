# maildo rules enable

Turn an Inbox rule on

## Usage

```
maildo rules enable <rule-id>
```

## Examples

```
maildo rules enable rule-id --account work
maildo rules enable rule-id --dry-run --json
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

## Preview changes

This command makes changes. Use `--dry-run` to see what would change without changing anything.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- rules disable
- rules view
