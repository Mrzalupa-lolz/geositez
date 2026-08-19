#!/usr/bin/env bash
# remnanode-geodata.sh — качает custom-geosite.dat / custom-geoip.dat из твоего
# GitHub-репозитория на сервер с remnanode и подкладывает их Xray.
#
#   sudo ./remnanode-geodata.sh install    # первичная установка + таймер обновления
#   sudo ./remnanode-geodata.sh update     # разово обновить (это же дёргает таймер)
#   sudo ./remnanode-geodata.sh status     # что лежит на диске
#
# Настройки можно переопределить переменными окружения, например:
#   REPO=ivan/geodata CONTAINER=remnanode sudo -E ./remnanode-geodata.sh install

set -euo pipefail

# ─────────── настройки ───────────
REPO="${REPO:-OWNER/REPO}"                       # <-- ВПИШИ СВОЙ репозиторий
FILES="${FILES:-custom-geosite.dat custom-geoip.dat}"
DEST="${DEST:-/opt/remnanode/geodata}"           # где файлы лежат на хосте
CONTAINER="${CONTAINER:-remnanode}"
COMPOSE_FILE="${COMPOSE_FILE:-/opt/remnanode/docker-compose.yml}"
RESTART="${RESTART:-1}"                          # 1 — рестартить ноду при изменении файлов
# Источник. Если GitHub с сервера недоступен — раскомментируй зеркало jsDelivr.
BASE_URL="${BASE_URL:-https://github.com/${REPO}/releases/latest/download}"
# BASE_URL="https://cdn.jsdelivr.net/gh/${REPO}@release"
SELF_PATH="/usr/local/bin/remnanode-geodata.sh"
XRAY_ASSET_DIR="/usr/local/share/xray"
# ─────────────────────────────────

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

need_root() { [ "$(id -u)" = "0" ] || die "запусти через sudo"; }

check_repo() {
  case "$REPO" in
    OWNER/REPO|*/REPO|"") die "не задан репозиторий: впиши REPO=твой-логин/твой-репо в скрипт или в окружение" ;;
  esac
}

# Скачивает один файл во временный, проверяет и кладёт на место.
# Возвращает 0 если файл изменился, 1 если остался прежним.
fetch_one() {
  local name="$1" url="${BASE_URL}/$1" tmp
  tmp="$(mktemp "${DEST}/.${name}.XXXXXX")"
  trap 'rm -f "$tmp"' RETURN

  curl -fsSL --retry 3 --retry-delay 2 --max-time 120 -o "$tmp" "$url" \
    || die "не скачался $url (репозиторий приватный? неверное имя? нет сети?)"

  # .dat — это protobuf, он всегда начинается с байта 0x0A и не бывает пустым
  [ -s "$tmp" ] || die "$name скачался пустым"
  [ "$(head -c 1 "$tmp" | od -An -tx1 | tr -d ' ')" = "0a" ] \
    || die "$name не похож на geodata-файл (пришла HTML-страница вместо .dat?)"

  if [ -f "${DEST}/${name}" ] \
     && [ "$(sha256sum <"$tmp" | cut -d' ' -f1)" = "$(sha256sum <"${DEST}/${name}" | cut -d' ' -f1)" ]; then
    log "$name — без изменений"
    return 1
  fi

  # Пишем содержимое поверх существующего файла (не mv!): файл примонтирован
  # в контейнер bind-mount'ом, и подмена через rename сломала бы монтирование.
  cat "$tmp" >"${DEST}/${name}"
  chmod 0644 "${DEST}/${name}"
  log "$name — обновлён ($(stat -c%s "${DEST}/${name}") байт)"
  return 0
}

restart_node() {
  [ "$RESTART" = "1" ] || { warn "RESTART=0 — нода не перезапущена, новые списки подхватятся позже"; return 0; }
  if [ -f "$COMPOSE_FILE" ] && docker compose version >/dev/null 2>&1; then
    log "перезапускаю ноду через docker compose"
    docker compose -f "$COMPOSE_FILE" restart "$CONTAINER"
  else
    log "перезапускаю контейнер $CONTAINER"
    docker restart "$CONTAINER" >/dev/null
  fi
}

cmd_update() {
  need_root; check_repo
  mkdir -p "$DEST"
  local changed=0 f
  for f in $FILES; do
    if fetch_one "$f"; then changed=1; fi
  done
  if [ "$changed" = "1" ]; then
    restart_node
    log "готово: списки обновлены"
  else
    log "готово: всё уже актуально"
  fi
}

cmd_status() {
  check_repo
  echo "репозиторий : $REPO"
  echo "источник    : $BASE_URL"
  echo "каталог     : $DEST"
  local f
  for f in $FILES; do
    if [ -f "${DEST}/${f}" ]; then
      printf '  %-24s %8s байт  %s\n' "$f" "$(stat -c%s "${DEST}/${f}")" "$(date -r "${DEST}/${f}" '+%F %T')"
    else
      printf '  %-24s НЕТ\n' "$f"
    fi
  done
  echo
  echo "внутри контейнера $CONTAINER:"
  docker exec "$CONTAINER" ls -la "$XRAY_ASSET_DIR" 2>/dev/null || warn "контейнер не запущен или не найден"
  echo
  systemctl status remnanode-geodata.timer --no-pager 2>/dev/null | head -5 || true
}

install_timer() {
  cat >/etc/systemd/system/remnanode-geodata.service <<EOF
[Unit]
Description=Обновление custom geodata для remnanode
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=oneshot
Environment=REPO=${REPO}
Environment=DEST=${DEST}
Environment=CONTAINER=${CONTAINER}
Environment=COMPOSE_FILE=${COMPOSE_FILE}
Environment=RESTART=${RESTART}
Environment=BASE_URL=${BASE_URL}
ExecStart=${SELF_PATH} update
EOF

  cat >/etc/systemd/system/remnanode-geodata.timer <<'EOF'
[Unit]
Description=Раз в час подтягивать свежие geodata-списки

[Timer]
OnBootSec=3min
OnUnitActiveSec=1h
RandomizedDelaySec=5min
Persistent=true

[Install]
WantedBy=timers.target
EOF

  systemctl daemon-reload
  systemctl enable --now remnanode-geodata.timer
  log "таймер поставлен: systemctl list-timers remnanode-geodata.timer"
}

cmd_install() {
  need_root; check_repo
  command -v docker >/dev/null || die "docker не найден"
  command -v curl   >/dev/null || die "нет curl: apt install -y curl"

  mkdir -p "$DEST"
  local f
  for f in $FILES; do fetch_one "$f" || true; done

  # положить сам скрипт в /usr/local/bin, чтобы systemd его звал
  if [ "$(readlink -f "$0")" != "$SELF_PATH" ]; then
    install -m 0755 "$0" "$SELF_PATH"
    log "скрипт скопирован в $SELF_PATH"
  fi

  install_timer

  # проверить, примонтированы ли файлы в контейнер
  local missing=0
  for f in $FILES; do
    docker exec "$CONTAINER" test -f "${XRAY_ASSET_DIR}/${f}" 2>/dev/null || missing=1
  done

  if [ "$missing" = "1" ]; then
    echo
    warn "файлы ещё не видны внутри контейнера — добавь монтирование в ${COMPOSE_FILE}:"
    echo
    echo "    volumes:"
    for f in $FILES; do
      echo "      - '${DEST}/${f}:${XRAY_ASSET_DIR}/${f}'"
    done
    echo
    echo "  ВАЖНО: монтировать только отдельные файлы, не всю папку —"
    echo "  том на каталог затрёт дефолтные geosite.dat/geoip.dat."
    echo
    echo "  потом:  docker compose -f ${COMPOSE_FILE} up -d --force-recreate"
  else
    log "файлы видны внутри контейнера — всё на месте"
  fi
}

case "${1:-update}" in
  install) cmd_install ;;
  update)  cmd_update ;;
  status)  cmd_status ;;
  *) die "использование: $0 {install|update|status}" ;;
esac
