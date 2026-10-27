# maildo rules delete

Delete an Inbox rule

## Usage

```
maildo rules delete <rule-id>
```

## Examples

```
maildo rules delete rule-id --account work --dry-run
maildo rules delete rule-id --confirm rule-id
maildo rules delete rule-id --confirm rule-id --json
```

## Flags

- `--account` Account to use (default: the active account)
- `--confirm` The rule's id again, to confirm the delete

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Preview changes

This command makes changes. Use `--dry-run` to see what would change without changing anything.

## Destructive

This command performs a destructive action and asks for confirmation before acting. Preview it first with `--dry-run`; outside a dry run it proceeds only after explicit confirmation, and in non-interactive mode (`--json`, `--no-prompt`, or no terminal) it requires a confirmation flag instead of blocking.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- rules list
- rules view
