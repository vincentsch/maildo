# Releasing maildo

This is how to make a release. A release is a tag `vX.Y.Z` on a commit that
passed every check below. The tag starts the release job, which publishes
the binaries.

## Check the candidate locally

Start from a clean checkout. You need:

- the Go version from `go.mod`
- Python 3 and Make
- Docker, for the test mail servers
- a C compiler, for the race detector

```sh
make release-check
make integration
```

`make integration` runs every IMAP and SMTP command against Dovecot and
GreenMail in Docker, and removes the containers afterwards.

Run the checks again in a fresh clone of the committed candidate, so that
no local files are involved. Look through `git ls-files` for anything
private or machine-specific. Before you publish, search for secrets and
read the matches:

```sh
git grep -n -i -E 'password|secret|token|BEGIN PRIVATE KEY'
```

Test passwords such as `secret` and `app-password` are expected. Read each
match, because a search alone does not prove that there are no secrets.

In `CHANGELOG.md`, rename `## Unreleased` to `## X.Y.Z - YYYY-MM-DD` with
the release date, and commit that before you tag. The tag is `vX.Y.Z`, and
`maildo --version` reports `X.Y.Z`. Check the notes that the release job
will use:

```sh
make release-notes VERSION=X.Y.Z
```

The release fails if the dated section is missing. Start a new
`## Unreleased` section for later work.

Build the four binaries from the committed candidate. Windows gets none
until maildo supports it.

```sh
make build-all VERSION=X.Y.Z COMMIT="$(git rev-parse HEAD)"
```

`dist/` then holds these files:

- `maildo-linux-amd64`
- `maildo-linux-arm64`
- `maildo-darwin-amd64`
- `maildo-darwin-arm64`
- `checksums.txt`

Run the binary for your own system with `--version`. Then log in to the
Docker test servers with it (`scripts/mailservers.sh env` prints their
settings) and try `doctor` and `list`. Look at the other binaries with
`file`. A binary that builds for a platform is not proof that it runs
there.

## Check the installer before you publish

Serve the built files the way a GitHub release does:

```sh
scripts/fake-release.sh dist /tmp/maildo-release-fixture vX.Y.Z
python3 -m http.server 8765 --bind 127.0.0.1 --directory /tmp/maildo-release-fixture
```

In another shell, install into a temporary folder:

```sh
INSTALL_DIR=/tmp/maildo-install MAILDO_VERSION=vX.Y.Z \
  MAILDO_DOWNLOAD_BASE=http://127.0.0.1:8765 sh install.sh
/tmp/maildo-install/maildo --version
```

Compare the version, the commit and the hash with the candidate.
`go test ./scripts` checks that the installer rejects wrong checksums, and
that it leaves an existing installation alone when a download fails. Stop
the server and delete the temporary folders afterwards.

## Publish

Publishing needs the owner's go-ahead. Once you have it, make the
repository public and turn on GitHub private vulnerability reporting. Check
that the
[reporting channel](https://github.com/vincentsch/maildo/security/advisories/new)
works before you announce anything.

Push main and check that CI passes on Ubuntu and macOS. That includes the
checks job, which also vets the Windows build, and the integration job.
Then create and push the tag for the commit you checked. The release job
runs CI again and builds the four binaries. It attests them, and it uploads
them with `checksums.txt` and the changelog section. Only that job can
write releases and attestations.

## Check the published release

Make sure the release has exactly the five files and the right notes.
Check where each binary came from:

```sh
gh attestation verify maildo-<os>-<arch> --repo vincentsch/maildo
```

From a fresh shell, check the public installer and `go install`:

```sh
curl -fsSL https://raw.githubusercontent.com/vincentsch/maildo/main/install.sh | sh
maildo --version
go install github.com/vincentsch/maildo/cmd/maildo@vX.Y.Z
```

Write down what you checked, and on which systems, below the release's
changelog entry or in the release notes.
