# maildo auth switch

Change the active account

## Usage

```
maildo auth switch <name>
```

## Examples

```
maildo auth switch work
maildo auth switch work --dry-run
maildo auth switch work --json
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

- auth list
- auth status
