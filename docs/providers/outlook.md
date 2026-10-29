# Outlook and Microsoft 365

maildo connects to Outlook.com and Microsoft 365 mailboxes through
Microsoft Graph, Microsoft's API. Microsoft no longer accepts passwords over
IMAP for these mailboxes, so signing in needs an app registration. That is
a small entry in Microsoft Entra that tells Microsoft which program asks for
access to your mail. maildo does not come with a shared one. You create your
own once, and it is free.

> **Status:** tested live on 2026-10-29 with a Microsoft 365 work account.
> These worked:
>
> - signing in, in the browser and with a device code
> - reading and searching mail, threads included
> - listing and downloading attachments
> - drafts, and a reply saved as a draft
> - sending a new message
> - read marks and flags
> - moving messages, and creating and deleting folders
>
> Not tested live yet:
>
> - personal Outlook.com and Hotmail accounts
> - sending a saved draft, which is also how replies and forwards are sent
> - changes to Inbox rules, and categories
> - changes to many messages at once
>
> Please report what you find.

## 1. Get a directory

Microsoft stores app registrations in a directory, which Microsoft also
calls a tenant.

- Work or school mailbox (Microsoft 365): your organization already has
  one. Your administrator may have to allow you to register apps, or do it
  for you.
- Personal Outlook.com or Hotmail account: you need a directory first.
  Microsoft no longer lets a personal account register apps on its own. If
  you open App registrations without a directory, you see "The ability to
  create applications outside of a directory has been deprecated". The
  usual way to get a directory is to [sign up for a free Azure
  account](https://azure.microsoft.com/pricing/purchase-options/azure-account)
  with your personal account. Microsoft may ask for a credit card during
  that sign-up to confirm who you are, so read its page about charges
  before you agree. The directory it creates is called **Default
  Directory**.

## 2. Register the app

1. Open the [Microsoft Entra admin center](https://entra.microsoft.com) and
   sign in. If you can reach more than one directory, pick the right one
   with the **Settings** icon in the top bar.
2. Go to **Entra ID > App registrations** and choose **New registration**.
3. Give it a name, for example `maildo`.
4. Under **Supported account types**, choose **Any Entra ID Tenant +
   Personal Microsoft accounts**. maildo signs in through Microsoft's
   `common` endpoint, which needs this setting. It also lets your personal
   account sign in, even though the app is in a directory.
5. Choose **Register**.

On the app's **Overview** page, copy the **Application (client) ID**. It
looks like `11111111-2222-3333-4444-555555555555`.

Then add the address that Microsoft sends you back to after you sign in:

1. In the app, open **Authentication**. On the **Redirect URI
   configuration** tab, choose **Add Redirect URI**.
2. Pick the **Mobile and desktop applications** tile.
3. Under **Custom redirect URIs**, enter `http://localhost` and choose
   **Configure**. During sign-in, maildo listens on a free port on your
   computer, and Microsoft accepts any port for `localhost`.

## 3. Allow device code sign-in (optional)

On a computer without a browser, maildo can show a code that you enter on
another device. To allow that, open **Authentication** in the app, then the
**Settings** tab, and set **Allow public client flows** to **Enabled**. In
older versions of the page, the switch is under **Advanced settings** and
says **Yes**.

## 4. Check the permissions

When you sign in, maildo asks for these delegated Microsoft Graph
permissions:

| Permission | What maildo uses it for |
| --- | --- |
| `Mail.ReadWrite` | read, search, file, flag and delete mail; drafts |
| `Mail.Send` | send mail |
| `MailboxSettings.ReadWrite` | read and change Inbox rules |
| `User.Read`, `openid`, `profile`, `offline_access` | learn your address and stay signed in |

You don't need to add them in Entra. Microsoft asks you to agree the first
time you sign in. In a work directory, an administrator may have to agree
for the whole organization.

## 5. Log in

```sh
maildo config set outlook.client_id 11111111-2222-3333-4444-555555555555
maildo login --provider outlook --name work
```

maildo prints the sign-in address, opens your browser and waits:

```text
Opening your browser to sign in. If it does not open, visit this address:
https://login.microsoftonline.com/common/oauth2/v2.0/authorize?client_id=...
Waiting for you to finish signing in (up to 3 minutes)...
```

If no browser opens, for example over SSH, open the address in a browser on
the same computer. After you approve, the browser shows a short
confirmation and maildo saves the tokens. On a computer with no browser at
all, add `--device-code`.

The client id is not a secret. maildo stores it in the config file, and all
your Outlook accounts share it. You can also pass it in the environment
variable `MAILDO_OUTLOOK_CLIENT_ID`.

## What works

These work on Outlook accounts:

- folders, search, list, read, threads and attachments
- drafts, send, reply and forward
- message changes such as archive or flag
- categories and Inbox rules

`maildo rules` and `maildo outlook` work only on Outlook.
`maildo categories` works on Outlook and, as your own labels, on the Gmail
API:

```sh
maildo rules list --account work
maildo rules health --account work          # rules with problems
maildo rules create --account work --from client@example.com --move-to Clients          # shows the rule
maildo rules create --account work --from client@example.com --move-to Clients --apply  # creates it
maildo categories set --id 3c1f2a7 --category Client --account work
```

maildo sends every Outlook message with Graph's sendMail. A draft goes out
as the copy maildo checked, and maildo deletes the draft afterwards. Graph
takes about 3 MB of attachments in one request, so maildo sends at most
3 MB of attachments in one message. That holds for `send --attach` and for
drafts. Send bigger files from Outlook itself.

Outlook writes replies and forwards itself. To show you the real recipients
and the quoted text before anything is sent, `reply --send --dry-run` and
`forward --send --dry-run` save the message as a draft first. The preview
prints the `maildo draft send` command for that draft.

Search uses Microsoft's own search, so it works like the search box in
Outlook.

A few things Outlook does its own way:

- A reply to a message in your Sent folder goes to you, because you are its
  sender. To write to the original recipients, change the draft with
  `maildo draft update --id <draft id> --clear-to --to <address>`.
- Outlook writes the subject of a reply itself, for example with `RE:`
  where the preview shows `Re:`.
- Outlook counts a few hundred extra bytes in an attachment's size. A
  downloaded file has its real size.

## Staying signed in

maildo refreshes the access token by itself. If Microsoft asks you to sign
in again, for example after a password change, commands stop with a hint to
run `maildo auth repair <name>`.

## Troubleshooting

- "The ability to create applications outside of a directory has been
  deprecated": your personal account has no directory yet. See step 1.
- AADSTS50011, redirect URI mismatch: the app's redirect URI must be
  `http://localhost`, under the **Mobile and desktop applications**
  platform.
- AADSTS7000218, client assertion or secret required: the app is set up as
  a web app. Add the **Mobile and desktop applications** platform as in
  step 2.
- Need admin approval: your organization does not let users agree to apps
  themselves. Ask an administrator to grant consent to your app
  registration.
- AADSTS700016, application not found: the client id is wrong, or the app
  was created in another directory and allows only that directory's
  accounts.
- "Outlook asked maildo to slow down": Microsoft lets one app send only four
  requests at a time to a mailbox. This happens when another program
  uses the same app registration and mailbox at the same moment. Wait a few
  seconds and run the command again.
