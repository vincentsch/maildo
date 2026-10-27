# maildo folders list

List folders with their message counts

## Usage

```
maildo folders list
```

## Examples

```
maildo folders list
maildo folders list --account work
maildo folders list --all-accounts
maildo folders list --json
maildo folders list --plain
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

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- folders view
