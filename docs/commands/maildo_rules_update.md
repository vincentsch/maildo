# maildo rules update

Change an Inbox rule; a condition you give replaces its old values

## Usage

```
maildo rules update <rule-id>
```

## Examples

```
maildo rules update rule-id --account work --subject-contains invoice --subject-contains receipt
maildo rules update rule-id --move-to Archive --apply
maildo rules update rule-id --no-mark-read --apply --json
```

## Flags

- `--account` Account to use (default: the active account)
- `--apply` Change the rule; without --apply maildo only shows what would change
- `--from` Match mail from this address (repeatable; comma lists accepted)
- `--has-attachments` Match messages with attachments
- `--header-contains` Match a message header that contains this text (repeatable; any value matches)
- `--mark-read` Mark matching messages as read
- `--move-to` Folder to move matching messages to
- `--name` Name of the rule
- `--no-has-attachments` Remove the has-attachments condition
- `--no-mark-read` Remove the mark-read action
- `--no-stop-processing` Remove the stop-processing action
- `--recipient-contains` Match a recipient address that contains this text (repeatable; any value matches)
- `--sender-contains` Match a sender address that contains this text (repeatable; any value matches)
- `--sequence` Place in the order rules run, from 1 to 2147483647; no other rule may use it
- `--stop-processing` Run no later rules on a message this rule matched
- `--subject-contains` Match a subject that contains this text (repeatable; any value matches, but different conditions must all match)

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Preview changes

This command changes things. Add `--dry-run` to see what it would change; nothing changes then.

## Request details

Add `--include-meta` to the JSON output to get the result as `{data, meta}`, where `meta` holds details about the request.

## Related commands

- rules view
- rules list
- folders list
