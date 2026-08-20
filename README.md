# geositez

Свои `.dat` файлы для Xray-core: пишешь домены в обычный `.txt`, GitHub Actions
компилирует их в `custom-geosite.dat`, ноды на серверах сами подтягивают свежую
версию, а клиенты забирают те же списки в своих форматах.

**Здесь только политика роутинга — что блокировать, что вести мимо VPN.**
Массовые списки РФ/РБ (десятки тысяч подсетей и доменов) не ведутся вручную,
а берутся готовыми из [roscomvpn-geoip](https://github.com/hydraponique/roscomvpn-geoip)
и [roscomvpn-geosite](https://github.com/hydraponique/roscomvpn-geosite).

```
data/geosite/ads.txt      ──┐
data/geosite/system.txt     │  go run ./tools/geogen   ┌─► custom-geosite.dat ─► нода (ext:)
data/geosite/twitch-ads.txt ├────── GitHub Actions ────┤─► clash/*.txt        ─► mihomo
data/geosite/ru.txt       ──┘                          └─► singbox/*.srs      ─► sing-box/Happ

                             roscomvpn-geoip   ─► roscom-geoip.dat   ─┐
                             roscomvpn-geosite ─► roscom-geosite.dat ─┴─► та же нода
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
| `RU` | `data/geosite/ru.txt` | добавка: российские сервисы вне зон `.ru/.su/.рф`, которых нет у roscomvpn | DIRECT (клиент) |
| `SYSTEM` | `data/geosite/system.txt` | пуши APNs/FCM, обновления ОС, NTP, OCSP | DIRECT (клиент) |
| `CONNECTIVITY` | `data/geosite/connectivity.txt` | проверка интернета (NCSI, captive portal) | клиент → VPN, нода → DIRECT |
| `AI` | `data/geosite/ai.txt` | сервисы, режущие по IP ноды | отдельный чистый outbound |
| `TWITCH-ADS` | `data/geosite/twitch-ads.txt` | токен и манифест Twitch — Source без рекламы | **В ТУННЕЛЬ** (клиент) |

Зоны `.ru`, `.su`, `.рф` в списки **не входят** — они ловятся регулярками на
клиенте, перечислять их поштучно бессмысленно.

### Разделение труда с roscomvpn

Всё, что можно взять готовым и свежим, берётся готовым:

| | ведём мы | берём у roscomvpn |
|---|---|---|
| подсети РФ/РБ | — | `roscom-geoip.dat`, ~15 000 CIDR, пересборка ежедневно |
| массовые домены РФ | — | `whitelist`, `category-ru` |
| сайты, режущие РФ-IP | — | `category-geoblock-ru` (им нужен туннель, а не direct) |
| посервисные списки | — | `apple`, `microsoft`, `steam`, `telegram`, `youtube`, ... |
| реклама и трекеры | `ADS` + `ADS-NETWORKS`, ~5500 доменов | у них `category-ads` — 2 домена, это не список блокировки |
| системные домены | `SYSTEM` (пуши, обновления, NTP, OCSP) | категории нет |
| проверка связи | `CONNECTIVITY` | категории нет |
| чистый outbound для AI | `AI` | нет (у них это часть `category-geoblock-ru` → просто в туннель) |
| Twitch без рекламы | `TWITCH-ADS` | есть `twitch-ads`, наш список — его копия |

Проверить, не появилось ли у них то, что мы ведём руками (и выкинуть из наших
списков), можно сравнением `data/geosite/*.txt` с их `data/*`.

**О чём помнить:** их `geoip` пересобирается ежедневно, а `geosite` — только по
пушу в репозиторий, и на момент написания последняя сборка была от 15 апреля
2026. IP-часть свежая, доменная может отставать. Плюс это чужой репозиторий:
если он исчезнет, отвалится массовый RU-роутинг, а наши `ADS`/`SYSTEM`/
`CONNECTIVITY` продолжат работать — ради этого свой сборщик и остаётся.

### Twitch: Source без рекламы

Реклама Twitch вшита в HLS-поток (SSAI), своего домена у неё нет — блокировкой
не убрать. Но запросы, от гео которых зависит выдача, живут на разных хостах,
и их разводят по разным маршрутам:

| что | куда | зачем |
|---|---|---|
| `gql.twitch.tv`, `usher.ttvnw.net`, `playlist.ttvnw.net` (`TWITCH-ADS`) | **в туннель** | с зарубежного IP Twitch отдаёт полную лестницу качества, включая Source |
| `twitch.tv`, `video-weaver`, `*.cloudfront.net` (категория `twitch` у roscomvpn) | **напрямую** | рекламного инвентаря на РФ нет — вставлять в поток нечего, и видео не жрёт трафик ноды |

Ломается любая из половин: уведёшь видео в туннель — вернётся реклама,
оставишь манифест напрямую — вернётся резаное качество.

**Правило `TWITCH-ADS` обязано стоять выше правила на `twitch`**: `gql.twitch.tv`
— поддомен `twitch.tv`, и общее правило перехватит его первым. Готовый порядок —
в `examples/mihomo-rules.yaml` и `examples/singbox-rules.json`.

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

### Проверка на дубликаты

Чтобы лишние строки не доживали до `.dat`, они вычищаются в исходниках:

```bash
go run ./tools/dupcheck          # показать и вернуть код 1, если что-то нашлось
go run ./tools/dupcheck -fix     # удалить найденное прямо в .txt
go run ./tools/dupcheck -cross   # + записи, попавшие сразу в РАЗНЫЕ категории
```

Что считается лишним (строку разбирает та же логика, что и сборщик):

| находка | пример |
|---|---|
| точный повтор | `pubmatic.com` дважды, в том числе через `include:` |
| поддомен под доменом | `ads.pubmatic.com` при наличии `pubmatic.com` |
| `full:` под доменом | `full:foo.com` при наличии `foo.com` |
| что угодно под `keyword:` | `evil.tracker.com` при наличии `keyword:track` |
| подсеть внутри сети | `10.1.2.0/24` внутри `10.1.0.0/16` |
| повторный `include:` | две строки `include:ADS-NETWORKS` |

Порядок строк в файле не важен: остаётся более широкое правило, даже если оно
идёт ниже. `-fix` не трогает строку, у которой есть свой `@атрибут`, — там
удаление меняло бы поведение, решай руками. Проверка стоит первым шагом в CI,
так что дубликаты не пройдут в сборку молча.

`-cross` — не про дубликаты, а про конфликты роутинга: один домен в `ADS`
(в блок) и в `RU` (в direct) — сработает то правило, что стоит выше.

### Что появляется на выходе

Релиз с тегом `latest` (перезаписывается каждой сборкой) плюс ветка `release`:

```
https://github.com/Mrzalupa-lolz/geositez/releases/latest/download/custom-geosite.dat
https://github.com/Mrzalupa-lolz/geositez/releases/latest/download/custom-geoip.dat
https://github.com/Mrzalupa-lolz/geositez/releases/latest/download/site-ads.txt      (mihomo)
https://github.com/Mrzalupa-lolz/geositez/releases/latest/download/site-ads.srs      (sing-box)
```

Зеркала, если с сервера GitHub не открывается:

```
https://cdn.jsdelivr.net/gh/Mrzalupa-lolz/geositez@release/custom-geosite.dat
https://raw.githubusercontent.com/Mrzalupa-lolz/geositez/release/custom-geosite.dat
```

> Репозиторий должен быть **публичным** — иначе серверы не скачают файлы без токена.

---

## 3. Как это попадает на ноды

На сервере с `remnanode`:

```bash
curl -fsSL -o remnanode-geodata.sh \
  https://raw.githubusercontent.com/Mrzalupa-lolz/geositez/main/scripts/remnanode-geodata.sh
chmod +x remnanode-geodata.sh
REPO=Mrzalupa-lolz/geositez sudo -E ./remnanode-geodata.sh install
```

Скрипт:

1. кладёт три файла в `/opt/remnanode/geodata/` — наш `custom-geosite.dat`
   плюс `roscom-geoip.dat` и `roscom-geosite.dat` (имена нарочно не
   `geoip.dat`/`geosite.dat`, чтобы не затереть штатные ассеты Xray);
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
    { "url": "https://github.com/Mrzalupa-lolz/geositez/releases/latest/download/custom-geosite.dat",
      "file": "custom-geosite.dat" },
    { "url": "https://github.com/Mrzalupa-lolz/geositez/releases/latest/download/custom-geoip.dat",
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
tools/geogen/main.go      сборщик .dat (Go, без зависимостей)
tools/dupcheck/main.go    поиск дубликатов в исходных списках
scripts/                  установка на ноду, обновление рекламного списка
examples/                 готовые куски роутинга
.github/workflows/        сборка и публикация
```

## 7. Грабли

1. **Порядок правил.** Первое совпавшее выигрывает. Три места, где это решает всё:
   `CONNECTIVITY` выше `SYSTEM` и чужого `apple` (иначе `captive.apple.com` уедет
   в direct); `TWITCH-ADS` выше `twitch` (иначе вернётся реклама); `ADS` выше
   чужого `whitelist` (иначе `mradx.net` пойдёт в direct вместо блока).
2. **Домен без префикса в конфиге Xray** это `keyword`, а не домен. В наших
   `.txt` наоборот — дефолт `domain:`, так удобнее.
3. **Не монтировать папку целиком** в `/usr/local/share/xray/` — затрёт штатные файлы.
4. **Приватный репозиторий** = ноды не скачают файлы (нужен токен).
5. **Пустая категория** пропускается со предупреждением, а правило `ext:` на неё
   уронит конфиг ноды — не оставляй пустых файлов.
6. **Чужие категории — чужая политика.** У roscomvpn `google-play` идёт в туннель,
   а там `mtalk.google.com` (пуши FCM), который у нас в `SYSTEM` на direct. Наши
   правила должны стоять выше, иначе пуши поедут через VPN и будут отваливаться
   при каждом реконнекте.
7. **Не копируй их списки к себе.** Скопированное перестаёт обновляться и сразу
   начинает конфликтовать: в чужом `apple`, например, лежат и `captive.apple.com`
   (это наш `CONNECTIVITY`), и рекламные `iad.apple.com`/`iadsdk.apple.com`
   (это наш `ADS`). Подключай их файл целиком и ссылайся на категорию через
   `ext:roscom-geosite.dat:apple`.
