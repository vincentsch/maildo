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

This command makes changes. Use `--dry-run` to see what would change without changing anything.

## Destructive

This command performs a destructive action and asks for confirmation before acting. Preview it first with `--dry-run`; outside a dry run it proceeds only after explicit confirmation, and in non-interactive mode (`--json`, `--no-prompt`, or no terminal) it requires a confirmation flag instead of blocking.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- draft read
- delete
