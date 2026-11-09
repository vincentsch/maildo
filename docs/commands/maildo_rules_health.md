# maildo rules health

Check Inbox rules for problems

## Usage

```
maildo rules health
```

## Examples

```
maildo rules health
maildo rules health --account work
maildo rules health --all-accounts
maildo rules health --json
maildo rules health --plain
```

## Flags

- `--account` Account to use; repeat it for more than one (default: the active account)
- `--all-accounts` Use all your accounts

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

- rules list
- rules view
- folders list
