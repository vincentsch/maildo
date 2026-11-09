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

This command changes things. Add `--dry-run` to see what it would change; nothing changes then.

## Confirmation

This command deletes or overwrites something, so it asks before it acts. Preview it with `--dry-run` first. It doesn't ask when there is no terminal or when you pass `--json` or `--no-prompt`; then it needs its confirmation flag.

## Request details

Add `--include-meta` to the JSON output to get the result as `{data, meta}`, where `meta` holds details about the request.

## Related commands

- rules list
- rules view
