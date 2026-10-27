# maildo list

List the newest messages in a folder

## Usage

```
maildo list [folder]
```

## Examples

```
maildo list
maildo list inbox --unread
maildo list --account work --json
maildo list Archive --all-accounts
maildo list inbox --plain
```

## Flags

- `--account` Account to use; repeat it for more than one (default: the active account)
- `--all-accounts` Use all your accounts
- `--limit` Messages per page (max 1000)
- `--page` Page of results to show, starting at 1
- `--unread` Show only unread messages

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --plain to get text without color or formatting.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- search
- folders list
