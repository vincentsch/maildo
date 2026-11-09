# maildo command reference

Handle your email from the terminal

## Commands

- [maildo archive](maildo_archive.md) - Archive messages
- [maildo attachments](maildo_attachments.md) - Work with the attachments of messages and drafts
- [maildo attachments add](maildo_attachments_add.md) - Add files to a draft
- [maildo attachments download](maildo_attachments_download.md) - Save a message's attachments to a folder
- [maildo attachments list](maildo_attachments_list.md) - List the attachments of a message or draft
- [maildo attachments remove](maildo_attachments_remove.md) - Remove an attached file from a draft
- [maildo auth](maildo_auth.md) - Manage your accounts and their logins
- [maildo auth list](maildo_auth_list.md) - List your accounts
- [maildo auth login](maildo_auth_login.md) - Log in to a mail account
- [maildo auth logout](maildo_auth_logout.md) - Delete the saved login of an account
- [maildo auth refresh](maildo_auth_refresh.md) - Renew an account's saved login without asking you
- [maildo auth remove](maildo_auth_remove.md) - Remove an account from maildo
- [maildo auth rename](maildo_auth_rename.md) - Rename an account
- [maildo auth repair](maildo_auth_repair.md) - Log in again when a saved login stopped working
- [maildo auth status](maildo_auth_status.md) - Show whether an account has a working login
- [maildo auth switch](maildo_auth_switch.md) - Change the active account
- [maildo categories](maildo_categories.md) - Show or replace message categories (on Gmail, your own labels)
- [maildo categories clear](maildo_categories_clear.md) - Remove categories from messages (on Gmail, your own labels)
- [maildo categories list](maildo_categories_list.md) - List the categories of a message (on Gmail, your own labels)
- [maildo categories set](maildo_categories_set.md) - Replace the categories of messages (on Gmail, your own labels)
- [maildo config](maildo_config.md) - Read and change maildo settings
- [maildo config get](maildo_config_get.md) - Show one setting
- [maildo config list](maildo_config_list.md) - Show all settings
- [maildo config set](maildo_config_set.md) - Change one setting
- [maildo copy](maildo_copy.md) - Copy messages to another folder (on Gmail, this adds a label)
- [maildo delete](maildo_delete.md) - Move messages to Trash
- [maildo doctor](maildo_doctor.md) - Check an account's servers, login and folders
- [maildo draft](maildo_draft.md) - Write, change and send drafts
- [maildo draft create](maildo_draft_create.md) - Save a new draft
- [maildo draft read](maildo_draft_read.md) - Show a draft
- [maildo draft send](maildo_draft_send.md) - Send a draft
- [maildo draft update](maildo_draft_update.md) - Change a draft
- [maildo flag](maildo_flag.md) - Flag messages or remove the flag
- [maildo flag clear](maildo_flag_clear.md) - Remove the flag from messages
- [maildo flag set](maildo_flag_set.md) - Flag messages
- [maildo folders](maildo_folders.md) - List and change folders
- [maildo folders create](maildo_folders_create.md) - Create a folder
- [maildo folders delete](maildo_folders_delete.md) - Delete an empty folder without subfolders (on Gmail, an unused label)
- [maildo folders list](maildo_folders_list.md) - List folders with their message counts
- [maildo folders rename](maildo_folders_rename.md) - Rename a folder
- [maildo folders view](maildo_folders_view.md) - Show one folder
- [maildo forward](maildo_forward.md) - Forward a message (saves a draft unless you add --send)
- [maildo junk](maildo_junk.md) - Move messages to the Junk folder
- [maildo list](maildo_list.md) - List the newest messages in a folder
- [maildo login](maildo_login.md) - Log in to a mail account
- [maildo mark](maildo_mark.md) - Mark messages as read or unread
- [maildo mark read](maildo_mark_read.md) - Mark messages as read
- [maildo mark unread](maildo_mark_unread.md) - Mark messages as unread
- [maildo move](maildo_move.md) - Move messages to another folder
- [maildo outlook](maildo_outlook.md) - Commands that only work with Outlook accounts
- [maildo outlook archive-policy](maildo_outlook_archive-policy.md) - Check Outlook archive and retention settings
- [maildo outlook archive-policy check](maildo_outlook_archive-policy_check.md) - Show which archive and retention settings maildo can see
- [maildo read](maildo_read.md) - Read a message
- [maildo reply](maildo_reply.md) - Reply to a message (saves a draft unless you add --send)
- [maildo restore](maildo_restore.md) - Move messages back from Trash or Junk (to the inbox, or to --to)
- [maildo rules](maildo_rules.md) - List and change Inbox rules (Outlook)
- [maildo rules create](maildo_rules_create.md) - Create an Inbox rule
- [maildo rules delete](maildo_rules_delete.md) - Delete an Inbox rule
- [maildo rules disable](maildo_rules_disable.md) - Turn an Inbox rule off
- [maildo rules enable](maildo_rules_enable.md) - Turn an Inbox rule on
- [maildo rules health](maildo_rules_health.md) - Check Inbox rules for problems
- [maildo rules list](maildo_rules_list.md) - List Inbox rules
- [maildo rules update](maildo_rules_update.md) - Change an Inbox rule; a condition you give replaces its old values
- [maildo rules view](maildo_rules_view.md) - Show one Inbox rule
- [maildo search](maildo_search.md) - Search your mail
- [maildo send](maildo_send.md) - Send a new message
- [maildo thread](maildo_thread.md) - Show the messages of a conversation, with the start of each
- [maildo update](maildo_update.md) - Check for and install the latest version

## Global flags

- `--config` Path to the config file
- `--dry-run` Preview changes without performing them
- `--include-meta` Wrap the JSON output as {data, meta} with request details
- `--jq` Filter the JSON output with a jq expression
- `--json` Output stable JSON instead of the human view
- `--no-ansi` Disable all ANSI/control sequences in human output
- `--no-color` Disable color in human output
- `--no-pager` Never use a pager for long human output
- `--no-prompt` Never block on an interactive prompt
- `--plain` Print text without color or formatting
- `--quiet` Suppress non-essential output
- `--template` Format the JSON output with a Go text/template
