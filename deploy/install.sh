#!/bin/sh -eu
# deploy/install.sh — install altrdb-telegram-bot under systemd.
#
#   sudo sh deploy/install.sh
#   sudo $EDITOR /etc/altrdb-telegram-bot/bot.env      # set the token
#   sudo systemctl enable --now altrdb-telegram-bot
#
# Idempotent: re-running it after an upgrade replaces the binary and the unit
# and restarts the service, leaving the database alone.
#
# Override any path with an environment variable:
#   PREFIX=/opt/altrdb sudo -E sh deploy/install.sh

set -eu

PREFIX=${PREFIX:-/usr/local}
ETCDIR=${ETCDIR:-/etc/altrdb-telegram-bot}
UNITDIR=${UNITDIR:-/etc/systemd/system}
BIN=${BIN:-bin/altrdb-telegram-bot}
SERVICE=altrdb-telegram-bot.service

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this as root (it writes to $PREFIX, $ETCDIR and $UNITDIR)"
[ -f "$BIN" ] || die "$BIN not found; run 'make build' first"
SRC_UNIT=deploy/systemd/$SERVICE
[ -f "$SRC_UNIT" ] || die "$SRC_UNIT not found"

log "installing the binary to $PREFIX/bin/"
install -D -m 0755 "$BIN" "$PREFIX/bin/altrdb-telegram-bot"

log "installing the systemd unit to $UNITDIR/"
install -D -m 0644 "$SRC_UNIT" "$UNITDIR/$SERVICE"

if [ ! -f "$ETCDIR/bot.env" ]; then
	log "creating $ETCDIR/bot.env from .env.example (mode 0600)"
	install -d -m 0750 "$ETCDIR"
	if [ -f .env.example ]; then
		install -m 0600 .env.example "$ETCDIR/bot.env"
	else
		: > "$ETCDIR/bot.env"
		chmod 0600 "$ETCDIR/bot.env"
	fi
	printf '    edit it now and set TELEGRAM_BOT_TOKEN\n'
else
	log "keeping the existing $ETCDIR/bot.env"
fi

log "reloading systemd"
systemctl daemon-reload

if systemctl is-active --quiet "$SERVICE"; then
	log "restarting the running service"
	systemctl restart "$SERVICE"
fi

log "done. Next steps:"
cat <<EOF
    \$EDITOR $ETCDIR/bot.env
    systemctl enable --now $SERVICE
    journalctl -u $SERVICE -f
    curl -s 127.0.0.1:8080/healthz
EOF
