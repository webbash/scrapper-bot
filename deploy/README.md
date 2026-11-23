# Запуск на VPS

Шаблоны для Debian/Ubuntu с systemd. `scrapper-bot.service` постоянно слушает Telegram, а cron запускает только `scrapper-bot-sync.service` в `:00` каждого часа. Ручная проверка запускает тот же одноразовый сервис: пока он работает, второй экземпляр не стартует. Cron не нужен для работы кнопок.

Секреты и полный URL находятся вне репозитория, в `/etc/scrapper-bot/bot.env` (`root:root`, `0600`). Оба сервиса выполняются от отдельного пользователя и используют один постоянный файл `/var/lib/scrapper-bot/housing.sqlite`. [.env в systemd и каталог состояния](https://manpages.debian.org/bookworm/systemd/systemd.exec.5.en.html) · [формат /etc/cron.d](https://manpages.debian.org/bookworm/cron/cron.8.en.html)

## 1. Собрать и перенести

На VPS узнайте архитектуру через `uname -m`: `x86_64` → `amd64`, `aarch64` → `arm64`. Соберите бинарник из корня проекта, например для `amd64`:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bazaraki-html-linux ./cmd/bazaraki-html
```

Перенесите `bazaraki-html-linux` и каталог `deploy/` на VPS по SSH/SCP. **Не копируйте локальный файл `.env` в открытый staging-каталог.**

## 2. Установить сервисы без расписания

Из папки, где лежат перенесённые файлы:

```sh
sudo apt-get update
sudo apt-get install -y cron ca-certificates
if ! id -u scrapper-bot >/dev/null 2>&1; then
  sudo useradd --system --user-group --no-create-home \
    --home-dir /var/lib/scrapper-bot --shell /usr/sbin/nologin scrapper-bot
fi
sudo install -d -o root -g root -m 0755 /opt/scrapper-bot
sudo install -m 0755 bazaraki-html-linux /opt/scrapper-bot/bazaraki-html
sudo install -d -o root -g root -m 0700 /etc/scrapper-bot
if ! sudo test -e /etc/scrapper-bot/bot.env; then
  sudo install -o root -g root -m 0600 deploy/.env.example /etc/scrapper-bot/bot.env
fi
sudoedit /etc/scrapper-bot/bot.env
sudo install -m 0644 deploy/systemd/*.service /etc/systemd/system/
sudo systemd-analyze verify /etc/systemd/system/scrapper-bot.service \
  /etc/systemd/system/scrapper-bot-sync.service
sudo systemctl daemon-reload
```

В `bot.env` укажите прежние `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID` и полный `BAZARAKI_SEARCH_URL` из локального конфига. URL вставьте в одинарных кавычках, включая `polygon` и остальные фильтры. `HOUSING_DB_PATH` оставьте как в шаблоне.

**Перед включением бота на VPS остановите локальный бот с тем же токеном.** Не запускайте локальный `sync` после переноса.

Если нужно сохранить историю и Фавориты, после остановки локального бота создайте снимок SQLite локально: `umask 077; sqlite3 housing.sqlite '.backup housing-vps.sqlite'`. Если база лежит в другом месте, укажите её путь. Передайте снимок на VPS в каталог, доступный только вам, и **до первого запуска** положите в каталог состояния. Не перезаписывайте уже используемую базу:

```sh
sudo install -d -o scrapper-bot -g scrapper-bot -m 0700 /var/lib/scrapper-bot
sudo test ! -e /var/lib/scrapper-bot/housing.sqlite &&
  sudo install -o scrapper-bot -g scrapper-bot -m 0600 \
    housing-vps.sqlite /var/lib/scrapper-bot/housing.sqlite
```

Если перенос истории не нужен, приложение само создаст новую SQLite при запуске сервиса.

```sh
sudo systemctl enable --now scrapper-bot.service
sudo journalctl -u scrapper-bot.service -n 30 --no-pager
```

Проверьте, что бот отвечает на `/favorites@ИмяБота` в группе. С согласованным доступом к источнику запустите один `sync` вручную и проверьте журнал. При новой базе первая успешная проверка сохранит текущую выдачу без рассылки; при переносе базы будут учитываться сохранённые ID.

```sh
sudo systemctl start scrapper-bot-sync.service
sudo journalctl -u scrapper-bot-sync.service -n 30 --no-pager
```

## 3. Включить часовой cron отдельно

После успешного ручного запуска и при разрешённом регулярном доступе к источнику:

```sh
sudo install -o root -g root -m 0644 deploy/cron.d/scrapper-bot-sync /etc/cron.d/scrapper-bot-sync
sudo systemctl enable --now cron.service
```

Cron использует часовой пояс VPS. При UTC или `Asia/Nicosia` запуск приходится на `:00` по времени Кипра. Если за час появилось несколько подходящих объявлений, они придут отдельными сообщениями подряд. Если новых нет, бот промолчит.

Чтобы отключить только расписание, удалите `/etc/cron.d/scrapper-bot-sync`. Бот продолжит отвечать на кнопки, а ручной `systemctl start scrapper-bot-sync.service` останется доступен.
