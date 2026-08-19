#!/usr/bin/env bash
# Пересобирает data/geosite/ads-networks.txt из публичных источников.
# Запускать руками раз в пару месяцев:  bash scripts/refresh-ads-networks.sh
set -euo pipefail

cd "$(dirname "$0")/.."
OUT="data/geosite/ads-networks.txt"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> качаю источники"
curl -fsSL -o "$TMP/pgl.txt" \
  "https://pgl.yoyo.org/adservers/serverlist.php?hostformat=plain&showintro=0&mimetype=plaintext"
curl -fsSL -o "$TMP/adg1.txt" \
  "https://raw.githubusercontent.com/AdguardTeam/AdguardFilters/master/MobileFilter/sections/adservers.txt"
curl -fsSL -o "$TMP/adg2.txt" \
  "https://raw.githubusercontent.com/AdguardTeam/AdguardFilters/master/SpywareFilter/sections/mobile.txt"

echo "==> собираю список"
python3 - "$TMP" "$OUT" <<'PY'
import os, re, sys

tmp, out = sys.argv[1], sys.argv[2]

PROTECTED = set("""
google.com googleapis.com gstatic.com googleusercontent.com youtube.com ytimg.com
googlevideo.com android.com gmail.com goo.gl google.ru
facebook.com fbcdn.net instagram.com whatsapp.com whatsapp.net messenger.com
twitter.com x.com twimg.com t.co
reddit.com redditmedia.com redditstatic.com
snapchat.com soundcloud.com sndcdn.com spotify.com scdn.co
tiktok.com tiktokcdn.com twitch.tv ttvnw.net jtvnw.net
discord.com discordapp.com discord.gg telegram.org t.me telegram.me telesco.pe
vk.com vk.me userapi.com mycdn.me mail.ru my.com ok.ru odnoklassniki.ru
yandex.ru yandex.net yandex.com yastatic.net ya.ru dzen.ru
apple.com icloud.com mzstatic.com akadns.net akamai.net akamaiedge.net akamaihd.net
microsoft.com windows.com windowsupdate.com live.com office.com office365.com bing.com
msftconnecttest.com msftncsi.com skype.com xbox.com
amazon.com amazonaws.com cloudfront.net aws.amazon.com
github.com githubusercontent.com gitlab.com
netflix.com nflxvideo.net steamcommunity.com steampowered.com epicgames.com
linkedin.com pinterest.com paypal.com ebay.com aliexpress.com aliexpress.ru
booking.com airbnb.com uber.com wikipedia.org cloudflare.com
sberbank.ru gosuslugi.ru wildberries.ru ozon.ru avito.ru tinkoff.ru tbank.ru
rutube.ru kinopoisk.ru hh.ru 2gis.ru rt.ru mts.ru megafon.ru beeline.ru
zoom.us slack.com dropbox.com adobe.com mozilla.org duckduckgo.com
""".split())

DENY = {"imasdk.googleapis.com", "googleads.g.doubleclick.net",
        "s.youtube.com", "static.doubleclick.net"}

DOMAIN_RE = re.compile(r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$")
RULE_RE = re.compile(r"^\|\|([a-z0-9.*-]+)\^(\$[a-z0-9,_~-]*)?$")

domains = set()
for line in open(os.path.join(tmp, "pgl.txt"), encoding="utf-8", errors="ignore"):
    d = line.strip().lower()
    if d and not d.startswith("#"):
        domains.add(d)

for f in ("adg1.txt", "adg2.txt"):
    for line in open(os.path.join(tmp, f), encoding="utf-8", errors="ignore"):
        line = line.strip()
        if not line or line.startswith(("!", "@@", "#")):
            continue
        m = RULE_RE.match(line)
        if not m:
            continue
        mod = m.group(2) or ""
        if "domain=" in mod or "denyallow" in mod:
            continue
        d = m.group(1).strip(".")
        if "*" not in d and "." in d:
            domains.add(d)

kept = [d for d in sorted(domains)
        if DOMAIN_RE.match(d) and d not in PROTECTED and d not in DENY]

kept_set = set(kept)
final = []
for d in kept:
    p = d.split(".")
    if any(".".join(p[i:]) in kept_set for i in range(1, len(p) - 1)):
        continue
    final.append(d)

header = """# ADS-NETWORKS — массовый импорт доменов рекламных сетей и in-app ad SDK.
# Собрано из двух публичных источников (оба содержат ТОЛЬКО рекламные/трекерные
# хосты, без доменов самих сервисов):
#   * pgl.yoyo.org/adservers  — классические ad-серверы
#   * AdguardFilters MobileFilter/adservers + SpywareFilter/mobile — SDK мобильной рекламы
# Апексы крупных сервисов (youtube.com, snapchat.com, vk.com, ...) отфильтрованы,
# чтобы блок рекламы не превратился в блок сервиса.
# Подключается автоматически: ads.txt делает include:ADS-NETWORKS.
#
# Обновить руками:  см. scripts/refresh-ads-networks.sh
"""

with open(out, "w", encoding="utf-8", newline="\n") as f:
    f.write(header + "\n" + "\n".join(final) + "\n")
print("готово:", len(final), "доменов ->", out)
PY

echo "==> проверяю сборку"
go run ./tools/geogen -data data -out dist >/dev/null
echo "OK. Не забудь: git add data && git commit && git push"
