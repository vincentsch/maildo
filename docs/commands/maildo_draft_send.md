# maildo draft send

Send a draft

## Usage

```
maildo draft send [id]
```

## Examples

```
maildo draft send 3c1f2a7
maildo draft send --id 3c1f2a7 --dry-run
maildo draft send --account work --id 3c1f2a7 --approve k7q2m9xd --yes
maildo draft send --id 3c1f2a7 --dry-run --full
```

## Flags

- `--account` Account to use (default: the active account)
- `--approve` Approval code from the --dry-run preview; maildo sends only if the message still matches it, and only once
- `--full` Show the whole body in the preview, also when it is long
- `--id` Draft id, or the short id maildo printed
- `--yes` Send without asking; needs --approve unless the account allows unattended sends

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Preview changes

This command makes changes. Use `--dry-run` to see what would change without changing anything.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- draft read
- send
