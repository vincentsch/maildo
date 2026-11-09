# maildo search

Search your mail

## Usage

```
maildo search <query>
```

## Examples

```
maildo search "from:client@example.com contract"
maildo search invoice --account work --account personal --json
maildo search "has:attachment after:2026-01-01" --all-accounts --limit 50
maildo search --folder Archive "from:vendor@example.com subject:invoice"
maildo search report --plain
```

## Flags

- `--account` Account to use; repeat it for more than one (default: the active account)
- `--all-accounts` Use all your accounts
- `--folder` Search only this folder; without it, every folder except Trash and Junk
- `--limit` Messages per page (max 1000)
- `--page` Page of results to show, starting at 1

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --plain to get text without color or formatting.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Request details

Add `--include-meta` to the JSON output to get the result as `{data, meta}`, where `meta` holds details about the request.

## Related commands

- list
- folders list
