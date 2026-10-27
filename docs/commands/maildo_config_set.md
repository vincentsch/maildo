# maildo config set

Change one setting

## Usage

```
maildo config set <setting> <value>
```

## Examples

```
maildo config set outlook.client_id 00000000-0000-0000-0000-000000000000
maildo config set gmail.client_id 1234-abc.apps.googleusercontent.com
maildo config set sender_name "Ann Example" --account work
maildo config set policy.drafts_only true --account work
maildo config set policy.allow_recipients "@example.com,boss@example.org" --account work
maildo config set policy.max_bulk 50 --account work
```

## Flags

- `--account` Account the setting is for (default: the active account)

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Preview changes

This command makes changes. Use `--dry-run` to see what would change without changing anything.

## Related commands

- config get
- config list
