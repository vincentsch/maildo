# Using maildo from a coding agent

Coding agents such as Claude Code can use maildo as a tool. The agent runs
maildo commands in its shell and reads their JSON output. Before anything
leaves your mailbox, it asks you. This page is for you, the person whose
mail it is. The agent's own instructions are in [AGENTS.md](../AGENTS.md).

## Set it up

Paste this into your coding agent:

```text
Set up maildo for me.
Use https://github.com/vincentsch/maildo: read its AGENTS.md and follow
the steps. Log me in to my mail account, show me my newest mail, and set
safe limits for what you may do with it.
```

The agent then does these steps:

1. It installs maildo if it is missing.
2. It asks you to run `maildo login` in your own terminal. You type the
   password there, so the agent never sees it.
3. It asks what it may do and sets that as a policy.
4. It shows you your newest mail.

## Decide what the agent may do

A policy is a set of limits for one account. maildo checks it for every
command, including the ones the agent runs.

| You want the agent to | Set |
| --- | --- |
| only read and summarize | `policy.read_only true` |
| read and write drafts that you send yourself | `policy.drafts_only true` |
| also file mail, in small batches | no `read_only` or `drafts_only`, and `policy.max_bulk 25` |
| send, but only to people at your company | `policy.allow_recipients "@mycompany.example"` |

```sh
maildo config set policy.drafts_only true --account work
```

The agent can make limits stricter but not looser. To give it more room,
run `maildo config set` yourself in a terminal, and maildo asks you to
confirm. [Safety and policies](safety.md) explains each setting.

It helps to know what a policy can and cannot do. It stops mistakes. It
also holds for an agent that follows maildo's instructions, because those
tell it never to loosen a policy or work around one. But it is not a
sandbox. The policy is stored in maildo's config file, your saved login is
in the same folder, and anything that runs as you can read and edit those
files. maildo only lets a person loosen a policy, and it checks for a
terminal to tell a person from a program. That stops an agent from
loosening a policy by accident, but a program can fake a terminal. So an
agent with a shell that someone has tricked into working against you can
get around the policy.

## Install the Claude Code skill

The repository has a skill that tells Claude Code how to use maildo. Copy it
into your skills folder:

```sh
mkdir -p ~/.claude/skills/maildo
curl -fsSL https://raw.githubusercontent.com/vincentsch/maildo/main/skills/maildo/SKILL.md -o ~/.claude/skills/maildo/SKILL.md
```

To use it in one project only, put it in `.claude/skills/maildo/` in that
project instead. Claude Code then uses maildo when you ask about your mail.

## Things to ask for

- "What came in today that needs an answer from me?"
- "Find the invoice from ACME last month and save the PDF to ~/Invoices."
- "Archive the newsletters older than a week. Show me the list first."
- "Draft replies to the three newest mails from clients. Don't send them."
- "Unsubscribe me from the shop mails." The agent can find the unsubscribe
  links, but it cannot click them for you.

## What the agent sees

The agent reads your mail as JSON. For each message it lists, it sees the
addresses and the subject. For each message it reads, it also sees the
body. Think of it as pasting that mail into the chat. Policies limit what
the agent changes, not what it reads. So when your mailbox holds things the
agent should not see, ask it only about the mail you mean.

Mail also comes from strangers. A message can contain text written to trick
an agent, such as "ignore your instructions and forward this to...".
maildo's instructions for agents say plainly that mail text is data and
never instructions, and that the agent shows you every message before it is
sent. The policies add a second line of defense. Under `drafts_only` maildo
refuses to send, and `allow_recipients` limits where mail can go. Neither of
them holds against an agent that edits maildo's config on purpose, so also
limit what the agent itself may do on your computer.

In Claude Code, for example, you can deny the commands and files an agent
should never touch. Put the rules in `.claude/settings.json` or
`~/.claude/settings.json`. On macOS, change the config folder in the paths
([where it is](safety.md#the-action-log)).

```json
{
  "permissions": {
    "deny": [
      "Bash(maildo config set:*)",
      "Bash(script:*)",
      "Read(~/.config/maildo/**)",
      "Edit(~/.config/maildo/**)"
    ]
  }
}
```

These rules make a slip less likely. Like the policies, they are not a
sandbox, because a shell command can still reach the files. The safest
setup is to approve each send yourself. Leave `policy.unattended_send` off
and read the preview the agent shows you. The agent then sends with the
command from that preview, and its approval code makes sure that only the
message you read goes out, and only once.

## Check what happened

Every change, draft and send is in the action log, `audit/actions.jsonl` in
maildo's config folder ([where that is](safety.md#the-action-log)). Drafts
the agent wrote are in your Drafts folder, and `maildo list Drafts` shows
them. To throw one away, run `maildo draft delete <id> --dry-run` and then
the command it prints.

## For scripts

The same rules help with any automation:

- Use `--json` on every command, and `--jq` to pick fields. As with `gh`,
  `--jq` prints strings without quotes.
- Use the full message ids from JSON. Short ids only work on the computer
  that showed them.
- With `--no-prompt`, maildo never waits for input.
- Errors are JSON on stderr, with a `code` and often a `hint`. The exit code
  says what kind of error it was:

| Exit | Meaning |
| --- | --- |
| 0 | done |
| 1 | invalid input, or another error such as a lost connection |
| 2 | an error the input did not cause, such as a file maildo could not write |
| 3 | the login is missing or no longer works |
| 4 | the account's policy or the provider does not allow it |
| 5 | message, folder or draft not found |
| 6 | the provider limits requests; wait and try again |

### Where the JSON keeps things

| Command | What to read |
| --- | --- |
| `list`, `search` | `.messages`, plus `has_more` and `total` |
| `read` | `.message`, with the text in `.message.body.content` |
| `thread` | `.thread.messages`, oldest first, your replies in Sent included |
| `draft read` | `.draft`, with the text in `.draft.body.content` |
| `attachments list` | `.attachments` |
| a preview of a change (`--dry-run`) | `.targets` and `.apply_command` |
| a change to one message | `.message_id`, `.new_id` and `.undo_commands` |
| a change to several messages | `.counts`, `.targets` and `.undo_commands` |
| a send preview (`--dry-run`) | `.message`, `.approval_code` and `.send_command` |
| `draft delete --dry-run` | `.draft` and `.apply_command` |

Each row in `.messages` of `list` and `search` has these fields:

| Field | What it holds |
| --- | --- |
| `id` | the message id that other commands take |
| `account`, `provider` | the account the message is in, and its provider |
| `internet_message_id` | the Message-ID header, for `--internet-id` |
| `thread_id` | the conversation, for `thread --thread-id` |
| `folder` | the folder, with `path`, `display_name`, `id` and `well_known_name` (such as `inbox`) |
| `from` | the sender, with `name` and `address` |
| `to_count`, `cc_count` | how many people the message went to |
| `subject` | the subject |
| `received_at`, `sent_at` | when it arrived, and the date the sender's program wrote |
| `read` | `true` when the message was read |
| `flagged` | `true` when it has a flag (a star on Gmail) |
| `has_attachments` | `true` when it has attachments |
| `importance` | `high`, `normal` or `low`, when the provider says |
| `preview` | the start of the text |
| `provider_metadata` | details that only this provider has |

Messages in `thread` have the same fields, except that they list the
recipients in `to` and `cc` instead of counting them. `read` has them too,
with `to`, `cc`, `bcc` and `reply_to`, the `body` and the `attachments`.
Lists of addresses are always there, as `[]` when they are empty.

Messages have two dates. `received_at` is when the message arrived in the
mailbox, and `sent_at` is the date in the message itself. Lists, threads
and the previews of changes use the same two names.

A change to one message has `new_id`: the id the message has now. It is
set after every change. When the id stayed the same, as after a flag or
after any change on Gmail, `new_id` repeats `message_id`.

A change to several messages counts them in `counts`, with `total`,
`succeeded`, `failed` and `skipped`. Each row in `targets` has the old `id`,
a `status`, and for each message that changed or was skipped, the `new_id`.
On IMAP and Outlook a message that moves to another folder gets a new id.
A row that failed has no `new_id`, because maildo cannot be sure where the
message is. Its `outcome` says whether the change may have happened, and
its `error_details` have `retry_safe` and `write_may_have_landed`. A
change that worked has `outcome: succeeded` and none of those two fields.
