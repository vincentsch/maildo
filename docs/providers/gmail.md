# Gmail

You can use a Gmail account with maildo in two ways.

| | App password over IMAP | Gmail API with your own OAuth app |
| --- | --- | --- |
| Setup | create an app password | create an OAuth app in Google Cloud |
| Needs | 2-Step Verification on your Google account | a Google Cloud project |
| Gmail labels as folders | yes, through IMAP | yes |
| Status | tested against real IMAP servers | not yet tested against live accounts |

For most people the app password is the better start.

## App password (recommended)

1. Turn on [2-Step Verification](https://myaccount.google.com/signinoptions/two-step-verification)
   for your Google account, if it is not on yet.
2. Create an app password at
   [myaccount.google.com/apppasswords](https://myaccount.google.com/apppasswords).
   Name it `maildo`. Google shows you a 16-letter password once.
3. Log in:

   ```sh
   maildo login --email you@gmail.com
   ```

   maildo knows Gmail's servers. Paste the app password when it asks for
   it.

In a script, put the password in `MAILDO_IMAP_PASSWORD` instead. Google
Workspace accounts work the same way if the administrator allows app
passwords.

Gmail's "All Mail" folder holds every message. When that folder exists,
maildo searches only there, so a message with three labels shows up once.

## Gmail API with your own OAuth app

This way uses Google's Gmail API instead of IMAP. Google requires every
program that reads Gmail through the API to have its own OAuth client, and
maildo does not come with a shared one.

### 1. Create the project and turn on the API

1. Open the [Google Cloud console](https://console.cloud.google.com) and
   create a project, for example `maildo`.
2. Go to **APIs & Services > Library**, search for **Gmail API** and choose
   **Enable**.

### 2. Set up the consent screen

1. Go to **APIs & Services > OAuth consent screen**. In newer versions of
   the console, it is **Google Auth Platform > Branding**.
2. Choose the **user type**:
   - **Internal** if you use Google Workspace and only people in your
     organization will sign in. Google does not need to review internal
     apps.
   - **External** for a personal `@gmail.com` account.
3. Fill in the app name and your email address.
4. Under **Data access** (or **Scopes**), add
   `https://www.googleapis.com/auth/gmail.modify`. maildo needs it to read,
   change and send mail.
5. For an External app in **Testing** status, add your own address under
   **Test users**.

### 3. Create the OAuth client

1. Go to **APIs & Services > Credentials > Create credentials > OAuth client
   ID**, or **Google Auth Platform > Clients > Create client**.
2. Choose **Desktop app** as the application type and create it.
3. Copy the **client ID**, which ends in `.apps.googleusercontent.com`, and
   the **client secret**. Google does not treat the secret of a desktop app
   as confidential. Still, maildo stores it only in its private credential
   file.

### 4. Log in

```sh
maildo config set gmail.client_id 1234-abcd.apps.googleusercontent.com
MAILDO_GMAIL_CLIENT_SECRET='GOCSPX-...' maildo login --provider gmail --name personal
```

maildo reads the client secret only from `MAILDO_GMAIL_CLIENT_SECRET`;
there is no `config set` key for it. You need it once, because maildo saves
it with the login, and later commands and `maildo auth repair` use the
saved copy. Without it, `maildo login` stops before it opens the browser and
tells you to set it.

maildo prints the sign-in address, opens your browser and waits on a local
port for Google's answer:

```text
Opening your browser to sign in. If it does not open, visit this address:
https://accounts.google.com/o/oauth2/v2/auth?client_id=...
Waiting for you to finish signing in (up to 5 minutes)...
```

If no browser opens, open the address in a browser on the same computer.
Google may warn that the app is unverified. It is your own app, so choose
**Continue**. If you see an **Advanced** link instead, which happens once
the app is In production, click it, then the link that starts with **Go to**
and ends with **(unsafe)**.

### The 7-day sign-in limit in Testing status

While an External app is in **Testing** status, Google ends each sign-in 7
days after you approve it. This applies to any app that asks for more than
your name, address and profile. maildo asks for `gmail.modify`, so it
applies to maildo, and maildo then asks you every week to run
`maildo auth repair <name>`. There are two ways around this:

- With Google Workspace, make the app **Internal**. The limit does not
  apply to internal apps.
- With a personal account, set the publishing status to **In production**,
  under **Google Auth Platform > Audience > Publish app** (in older
  consoles **OAuth consent screen > Publish app**). Google still shows the
  "unverified app" warning at sign-in, and allows up to 100 users without a
  review, which is enough for your own accounts. Sign-ins then stay valid.

These are Google's rules at the time of writing. If the console looks
different, check Google's pages on
[the app audience](https://support.google.com/cloud/answer/15549945) and
[OAuth](https://developers.google.com/identity/protocols/oauth2).

### Removing access

`maildo auth logout <name>` deletes the saved login on your computer, and
`maildo auth remove <name>` removes the account from maildo. Google still
lists your app as allowed after that. To take its access away, open
[Linked apps](https://myaccount.google.com/connections) in your Google
account, find your app and remove its access.

### Troubleshooting

- Error 403: access_denied: the app is in Testing, and your address is not
  listed under Test users.
- redirect_uri_mismatch: the OAuth client is not of the type **Desktop
  app**.
- invalid_client: the client secret is missing or wrong. Set
  `MAILDO_GMAIL_CLIENT_SECRET` and run `maildo auth repair <name>`.
