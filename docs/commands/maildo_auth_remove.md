# maildo auth remove

Remove an account from maildo

## Usage

```
maildo auth remove <name>
```

## Examples

```
maildo auth remove old
maildo auth remove work --set-active personal
maildo auth remove work --force --dry-run
maildo auth remove old --json
```

## Flags

- `--force` Remove the active account, and leave no account active
- `--set-active` Account to make active after the removal

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Preview changes

This command changes things. Add `--dry-run` to see what it would change; nothing changes then.

## Request details

Add `--include-meta` to the JSON output to get the result as `{data, meta}`, where `meta` holds details about the request.

## Related commands

- auth logout
- auth list
