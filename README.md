# geositez

Свои `.dat` файлы для Xray-core: пишешь домены в обычный `.txt`, GitHub Actions
компилирует их в `custom-geosite.dat` / `custom-geoip.dat`, ноды на серверах сами
подтягивают свежую версию, а клиенты забирают те же списки в своих форматах.

```
data/geosite/ads.txt   ──┐
data/geosite/ru.txt      │   go run ./tools/geogen      ┌─► custom-geosite.dat ─► нода (ext:)
data/geosite/system.txt  ├──────── GitHub Actions ──────┤─► custom-geoip.dat  ─► нода (ext:)
data/geoip/ru-ip.txt   ──┘                              ├─► clash/*.txt       ─► mihomo
                                                        └─► singbox/*.srs     ─► sing-box/Happ
```

---

## 1. Как добавлять домены

Один домен — одна строка. Файл в `data/geosite/` = категория, имя файла = имя
категории в верхнем регистре (`ads.txt` → `ADS`).

```text
# комментарий
example.com                 # домен И все его поддомены (это дефолт)
full:only-this.example.com  # ТОЛЬКО этот хост, без поддоменов
keyword:adserver            # подстрока — ловит широко, осторожнее
regexp:.*\.xn--p1ai$        # регулярка (дороже по CPU)
include:OTHER-LIST          # подмешать другую категорию целиком
https://site.com/path/?a=1  # можно вставить и ссылку — вырежется site.com
```

Что делает сборщик за тебя: приводит к нижнему регистру, срезает `https://`,
`www.`-мусор из ссылок, `*.`, порт и путь, выкидывает дубликаты, ругается на
кириллические домены (их нужно писать в punycode: `сайт.рф` → `xn--80aswg.xn--p1ai`).

Файлы в `data/geoip/` — то же самое, но строки это CIDR (`77.88.0.0/18`) или
одиночные IP (`8.8.4.4` превратится в `/32`).

### Готовые категории

| Категория | Файл | Что внутри | Куда в роутинге |
|---|---|---|---|
| `ADS` | `data/geosite/ads.txt` | реклама и трекеры по площадкам + `ADS-NETWORKS` | **BLOCK** |
| `ADS-NETWORKS` | `data/geosite/ads-networks.txt` | ~5300 доменов рекламных сетей и мобильных ad-SDK | подмешан в `ADS` |
| `RU` | `data/geosite/ru.txt` | российские сервисы **вне** зон `.ru/.su/.рф` | DIRECT (клиент) |
| `SYSTEM` | `data/geosite/system.txt` | пуши APNs/FCM, обновления ОС, NTP, OCSP | DIRECT (клиент) |
| `CONNECTIVITY` | `data/geosite/connectivity.txt` | проверка интернета (NCSI, captive portal) | клиент → VPN, нода → DIRECT |
| `AI` | `data/geosite/ai.txt` | сервисы, режущие по IP ноды | отдельный чистый outbound |
| `RU-IP` | `data/geoip/ru-ip.txt` | сети Яндекса и VK | DIRECT (клиент) |

Зоны `.ru`, `.su`, `.рф` в списки **не входят** — они ловятся регулярками на
клиенте, перечислять их поштучно бессмысленно.

---

## 2. Как собрать

**Вариант «нажал кнопку»:** GitHub → вкладка **Actions** → workflow **build geodata**
→ **Run workflow**.

**Вариант «само»:** любой push с изменениями в `data/**` запускает сборку сам.

**Локально проверить перед пушем** (нужен Go):

```bash
go run ./tools/geogen -data data -out dist
```

Сборка падает с понятной ошибкой, если в списке опечатка — битый файл до
серверов не доедет. В CI дополнительно скачивается настоящий Xray-core и
конфиг с ссылками на **каждую** категорию прогоняется через `xray run -test`.

### Что появляется на выходе

Релиз с тегом `latest` (перезаписывается каждой сборкой) плюс ветка `release`:

```
https://github.com/OWNER/geositez/releases/latest/download/custom-geosite.dat
https://github.com/OWNER/geositez/releases/latest/download/custom-geoip.dat
https://github.com/OWNER/geositez/releases/latest/download/site-ads.txt      (mihomo)
https://github.com/OWNER/geositez/releases/latest/download/site-ads.srs      (sing-box)
```

Зеркала, если с сервера GitHub не открывается:

```
https://cdn.jsdelivr.net/gh/OWNER/geositez@release/custom-geosite.dat
https://raw.githubusercontent.com/OWNER/geositez/release/custom-geosite.dat
```

> Репозиторий должен быть **публичным** — иначе серверы не скачают файлы без токена.

---

## 3. Как это попадает на ноды

На сервере с `remnanode`:

```bash
curl -fsSL -o remnanode-geodata.sh \
  https://raw.githubusercontent.com/OWNER/geositez/main/scripts/remnanode-geodata.sh
chmod +x remnanode-geodata.sh
REPO=OWNER/geositez sudo -E ./remnanode-geodata.sh install
```

Скрипт:

1. кладёт `.dat` в `/opt/remnanode/geodata/`;
2. показывает, какие строки добавить в `docker-compose.yml` (монтировать нужно
   **по одному файлу**, том на всю папку затрёт дефолтные `geosite.dat`/`geoip.dat`);
3. ставит systemd-таймер `remnanode-geodata.timer` — раз в час проверяет релиз,
   и **только если файл реально изменился** обновляет его и перезапускает ноду.

Полезное:

```bash
sudo remnanode-geodata.sh status    # что лежит, когда обновлялось, жив ли таймер
sudo remnanode-geodata.sh update    # подтянуть прямо сейчас
systemctl list-timers remnanode-geodata.timer
journalctl -u remnanode-geodata.service -n 50
```

### Альтернатива без рестартов

Xray умеет качать гео-файлы сам и перечитывать их на лету — блок `geodata`
в конфиге профиля:

```json
"geodata": {
  "cron": "0 */6 * * *",
  "assets": [
    { "url": "https://github.com/OWNER/geositez/releases/latest/download/custom-geosite.dat",
      "file": "custom-geosite.dat" },
    { "url": "https://github.com/OWNER/geositez/releases/latest/download/custom-geoip.dat",
      "file": "custom-geoip.dat" }
  ]
}
```

Плюс: обновление без перезапуска ноды. Минусы: файлы должны существовать на
момент старта ядра (иначе конфиг не загрузится), и **это несовместимо с
монтированием тех же файлов** — Xray подменяет файл через `rename`, а поверх
bind-mount так нельзя. Либо таймер + монтирование, либо `geodata` без монтирования.

---

## 4. Как включить в роутинг

### Серверный (Config Profile ноды в панели)

Полный пример — `examples/node-routing.json`. Суть:

```json
{ "type": "field", "domain": ["ext:custom-geosite.dat:CONNECTIVITY"], "outboundTag": "DIRECT" },
{ "type": "field", "domain": ["ext:custom-geosite.dat:ADS"],          "outboundTag": "BLOCK"  },
{ "type": "field", "ip":     ["geoip:private"],                        "outboundTag": "BLOCK"  }
```

Синтаксис ссылки — `ext:<имя файла>:<КАТЕГОРИЯ>`. Регистр категории не важен,
Xray сам приводит к верхнему. Обычные `geosite:` / `geoip:` продолжают работать:
дефолтные файлы на месте, наши лежат рядом отдельными файлами.

### Клиентский

* **mihomo / Clash** → `examples/mihomo-rules.yaml` (rule-provider'ы по URL);
* **sing-box / Happ** → `examples/singbox-rules.json` (`.srs` по URL);
* **чистый Xray-клиент** не умеет `ext:` без файла на устройстве — там списки
  либо вшиваются в шаблон подписки, либо клиент берёт mihomo/sing-box формат.

Порядок правил на клиенте:

```
CONNECTIVITY → в туннель     (нода отправит их в direct → честный пинг через VPN)
ADS          → REJECT
SYSTEM       → direct        (пуши и обновления мимо VPN)
RU, RU-IP    → direct
.ru/.su/.рф  → direct        (регулярками)
остальное    → в туннель
```

---

## 5. Про рекламу — что реально блокируется

В `ads.txt` только рекламные и трекерные хосты, доменов самих сервисов там нет:
блокируется `promoted.soundcloud.com`, а не `soundcloud.com`; `adsapi.snapchat.com`,
а не `snapchat.com`. Поэтому сервисы не ломаются.

Чего **не** уберёт блокировка по домену, физически:

* **YouTube pre-roll** — видеореклама идёт с `googlevideo.com`, того же хоста, что и само видео;
* **Twitch** — реклама вшита в HLS-поток (SSAI), отдельного домена нет;
* **Instagram/Facebook лента** — реклама приходит тем же graph API, что и посты;
* **Spotify аудио-реклама** — тот же CDN, что и музыка;
* **Telegram** — спонсорские сообщения внутри MTProto.

Всё остальное (баннеры, ad-SDK в мобильных приложениях, трекеры, myTarget,
Яндекс.Директ, TikTok Pangle, Snapchat Ads) режется.

Обновить массовый список из первоисточников:

```bash
bash scripts/refresh-ads-networks.sh
```

---

## 6. Структура репозитория

```
data/geosite/*.txt        исходные списки доменов
data/geoip/*.txt          исходные списки сетей
tools/geogen/main.go      сборщик .dat (Go, без зависимостей)
scripts/                  установка на ноду, обновление рекламного списка
examples/                 готовые куски роутинга
.github/workflows/        сборка и публикация
```

## 7. Грабли

1. **Порядок правил.** Первое совпавшее выигрывает. `CONNECTIVITY` — выше `SYSTEM`.
2. **Домен без префикса в конфиге Xray** это `keyword`, а не домен. В наших
   `.txt` наоборот — дефолт `domain:`, так удобнее.
3. **Не монтировать папку целиком** в `/usr/local/share/xray/` — затрёт штатные файлы.
4. **Приватный репозиторий** = ноды не скачают файлы (нужен токен).
5. **Пустая категория** пропускается со предупреждением, а правило `ext:` на неё
   уронит конфиг ноды — не оставляй пустых файлов.
