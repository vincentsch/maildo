# maildo auth refresh

Renew an account's saved login without asking you

## Usage

```
maildo auth refresh <name>
```

## Examples

```
maildo auth refresh work
maildo auth refresh work --dry-run
maildo auth refresh work --json
```

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Preview changes

This command makes changes. Use `--dry-run` to see what would change without changing anything.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- auth status
- auth repair
