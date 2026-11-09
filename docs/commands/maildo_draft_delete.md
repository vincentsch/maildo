# maildo draft delete

Discard a draft for good

## Usage

```
maildo draft delete [id]
```

## Examples

```
maildo draft delete 3c1f2a7 --dry-run
maildo draft delete 3c1f2a7 --confirm 3c1f2a7
maildo draft delete --id 3c1f2a7 --account work --dry-run --json
```

## Flags

- `--account` Account to use (default: the active account)
- `--confirm` The draft's id again, to confirm; --dry-run prints the command
- `--full` Show the whole body in the preview, also when it is long
- `--id` Draft id, or the short id maildo printed

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

- draft read
- delete
