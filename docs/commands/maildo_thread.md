# maildo thread

Show the messages of a conversation, with the start of each

## Usage

```
maildo thread [id]
```

## Examples

```
maildo thread 4d1a2f3
maildo thread --id 3c1f2a7
maildo thread --thread-id AAQk...
maildo thread --query "subject:contract" --first --account work --json
```

## Flags

- `--account` Account to use; repeat it for more than one (default: the active account)
- `--all-accounts` Use all your accounts
- `--first` With --query, use the first match; matches come newest first, so this is the same as --latest
- `--id` Message id, or a short id from list or search
- `--internet-id` Message-ID header of the message, such as <id@example.com>
- `--latest` With --query, use the newest match
- `--query` Search for the message; use with --latest or --first
- `--thread-id` Thread or conversation id from the provider, as in --json output

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- read
- search
- list
