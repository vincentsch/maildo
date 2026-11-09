# maildo auth list

List your accounts

## Usage

```
maildo auth list
```

## Examples

```
maildo auth list
maildo auth list --json
```

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Request details

Add `--include-meta` to the JSON output to get the result as `{data, meta}`, where `meta` holds details about the request.

## Related commands

- auth status
- auth login
