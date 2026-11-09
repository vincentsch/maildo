# maildo auth rename

Rename an account

## Usage

```
maildo auth rename <old> <new>
```

## Examples

```
maildo auth rename work office
maildo auth rename work office --dry-run
maildo auth rename work office --json
```

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

- auth list
- auth switch
