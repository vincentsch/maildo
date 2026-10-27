#!/usr/bin/env bash
# Start, stop or test against throwaway Dovecot and GreenMail servers in Docker.
#
#   scripts/mailservers.sh up     start both servers with a fresh test CA
#   scripts/mailservers.sh env    print the environment the integration tests read
#   scripts/mailservers.sh test   start the servers, run the integration tests, stop
#   scripts/mailservers.sh down   stop and remove both containers
#
# Both servers accept any user name, so each test run can use a fresh
# mailbox. Dovecot wants the password "secret"; GreenMail takes any password.
# Dovecot has SPECIAL-USE folders (Archive, Drafts, Junk, Sent, Trash) and
# CONDSTORE. GreenMail has neither, which makes it a good stand-in for small
# or older servers. GreenMail's SMTP server delivers to its own mailboxes and
# serves as the outgoing server for both.
set -euo pipefail

cd "$(dirname "$0")/.."
work=${MAILDO_IT_DIR:-$PWD/.maildo/mailservers}
dovecot_image=dovecot/dovecot:2.3.21
greenmail_image=greenmail/standalone:2.1.2
# Container names and host ports. Override them if they clash with
# something on your machine.
: "${MAILDO_IT_DOVECOT_NAME:=maildo-dev-dovecot}"
: "${MAILDO_IT_GREENMAIL_NAME:=maildo-dev-greenmail}"
dovecot_name=$MAILDO_IT_DOVECOT_NAME
greenmail_name=$MAILDO_IT_GREENMAIL_NAME
: "${MAILDO_IT_DOVECOT_IMAPS:=39993}"
: "${MAILDO_IT_DOVECOT_IMAP:=39143}"
: "${MAILDO_IT_GREENMAIL_IMAPS:=39994}"
: "${MAILDO_IT_GREENMAIL_SMTPS:=39465}"

make_certs() {
  mkdir -p "$work/certs"
  local d=$work/certs
  [[ -s $d/server.pem && -s $d/greenmail.p12 ]] && return
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=maildo test CA" \
    -keyout "$d/ca.key" -out "$d/ca.pem" 2>/dev/null
  openssl req -newkey rsa:2048 -nodes -subj "/CN=localhost" \
    -keyout "$d/server.key" -out "$d/server.csr" 2>/dev/null
  printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nbasicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' >"$d/ext.cnf"
  openssl x509 -req -in "$d/server.csr" -CA "$d/ca.pem" -CAkey "$d/ca.key" -CAcreateserial \
    -days 30 -extfile "$d/ext.cnf" -out "$d/server.pem" 2>/dev/null
  openssl pkcs12 -export -inkey "$d/server.key" -in "$d/server.pem" -name greenmail \
    -passout pass:changeit -out "$d/greenmail.p12" 2>/dev/null
  chmod 644 "$d"/*
}

write_dovecot_conf() {
  cat >"$work/dovecot.conf" <<'CONF'
mail_home=/srv/mail/%Lu
mail_location=maildir:~/Maildir
mail_uid=1000
mail_gid=1000
protocols = imap
first_valid_uid = 1000
last_valid_uid = 1000
auth_mechanisms = plain login
disable_plaintext_auth = yes
passdb {
  driver = static
  args = password=secret
}
userdb {
  driver = static
  args = uid=1000 gid=1000 home=/srv/mail/%Lu
}
ssl = required
ssl_cert = </certs/server.pem
ssl_key = </certs/server.key
namespace inbox {
  inbox = yes
  separator = /
  mailbox Drafts {
    special_use = \Drafts
    auto = subscribe
  }
  mailbox Junk {
    special_use = \Junk
    auto = subscribe
  }
  mailbox Trash {
    special_use = \Trash
    auto = subscribe
  }
  mailbox Sent {
    special_use = \Sent
    auto = subscribe
  }
  mailbox Archive {
    special_use = \Archive
    auto = subscribe
  }
}
service imap-login {
  inet_listener imap {
    port = 143
  }
  inet_listener imaps {
    port = 993
    ssl = yes
  }
}
protocol imap {
  mail_max_userip_connections = 100
}
listen = *
log_path = /dev/stdout
CONF
}

wait_for_port() {
  local port=$1
  for _ in $(seq 1 120); do
    if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  echo "Nothing is listening on port $port after 60 seconds." >&2
  return 1
}

up() {
  make_certs
  write_dovecot_conf
  down >/dev/null 2>&1 || true
  docker run -d --name "$dovecot_name" \
    -p "127.0.0.1:$MAILDO_IT_DOVECOT_IMAPS:993" -p "127.0.0.1:$MAILDO_IT_DOVECOT_IMAP:143" \
    -v "$work/dovecot.conf:/etc/dovecot/dovecot.conf:ro" -v "$work/certs:/certs:ro" \
    "$dovecot_image" >/dev/null
  docker run -d --name "$greenmail_name" \
    -p "127.0.0.1:$MAILDO_IT_GREENMAIL_IMAPS:3993" -p "127.0.0.1:$MAILDO_IT_GREENMAIL_SMTPS:3465" \
    -v "$work/certs:/certs:ro" \
    -e GREENMAIL_OPTS="-Dgreenmail.setup.test.all -Dgreenmail.hostname=0.0.0.0 -Dgreenmail.auth.disabled -Dgreenmail.tls.keystore.file=/certs/greenmail.p12 -Dgreenmail.tls.keystore.password=changeit" \
    "$greenmail_image" >/dev/null
  wait_for_port "$MAILDO_IT_DOVECOT_IMAPS"
  wait_for_port "$MAILDO_IT_GREENMAIL_IMAPS"
  wait_for_port "$MAILDO_IT_GREENMAIL_SMTPS"
  # GreenMail opens its ports before its user is ready; give it a moment.
  sleep 2
  echo "Dovecot IMAPS on 127.0.0.1:$MAILDO_IT_DOVECOT_IMAPS, GreenMail IMAPS on 127.0.0.1:$MAILDO_IT_GREENMAIL_IMAPS and SMTPS on 127.0.0.1:$MAILDO_IT_GREENMAIL_SMTPS."
  echo "Test CA: $work/certs/ca.pem"
}

down() {
  docker rm -f "$dovecot_name" "$greenmail_name" >/dev/null 2>&1 || true
}

print_env() {
  cat <<ENV
export MAILDO_IT_CA_FILE=$work/certs/ca.pem
export MAILDO_IT_DOVECOT_IMAPS=$MAILDO_IT_DOVECOT_IMAPS
export MAILDO_IT_DOVECOT_IMAP=$MAILDO_IT_DOVECOT_IMAP
export MAILDO_IT_GREENMAIL_IMAPS=$MAILDO_IT_GREENMAIL_IMAPS
export MAILDO_IT_GREENMAIL_SMTPS=$MAILDO_IT_GREENMAIL_SMTPS
ENV
}

run_tests() {
  up
  trap down EXIT
  eval "$(print_env)"
  go test -tags integration -count=1 ./internal/integration/... "$@"
}

case ${1:-} in
  up) up ;;
  down) down ;;
  env) print_env ;;
  test) shift; run_tests "$@" ;;
  *) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
