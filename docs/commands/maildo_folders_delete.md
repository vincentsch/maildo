# maildo folders delete

Delete an empty folder without subfolders (on Gmail, an unused label)

## Usage

```
maildo folders delete <folder>
```

## Examples

```
maildo folders delete "Old Projects" --account work --dry-run
maildo folders delete old-projects-id --confirm old-projects-id
maildo folders delete "Clients/2024" --confirm "Clients/2024" --json
```

## Flags

- `--account` Account to use (default: the active account)
- `--confirm` The folder's id, or its full path, again to confirm the delete; --dry-run prints the command

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

- folders list
- folders view
