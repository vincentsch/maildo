# maildo auth logout

Delete the saved login of an account

## Usage

```
maildo auth logout <name>
```

## Examples

```
maildo auth logout work
maildo auth logout work --dry-run
maildo auth logout work --json
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

- auth login
- auth status
