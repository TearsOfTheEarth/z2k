#!/bin/sh
# Preserve the independent opt-in nfqws2 autohostlist; retire experimental discovery.

PASS=0; FAIL=0
ok() { PASS=$((PASS+1)); printf '[PASS] %s\n' "$1"; }
no() { FAIL=$((FAIL+1)); printf '[FAIL] %s (want=%s got=%s)\n' "$1" "$2" "$3"; }

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
INST="$ROOT/lib/install.sh"
[ -f "$INST" ] || { printf '[FAIL] нет %s\n' "$INST"; exit 1; }

TMP=$(mktemp -d) || exit 1
trap 'chmod -R u+w "$TMP" 2>/dev/null; rm -rf "$TMP"' EXIT INT TERM

MARK='for _acc in autohostlist-domains; do'

# Вынимаем НАСТОЯЩИЕ куски установщика: первый цикл — бэкап, второй — возврат.
extract() {
    awk -v want="$1" -v mark="$MARK" '
        index($0, mark) { n++; if (n == want) grab = 1 }
        grab { print }
        grab && /^[[:space:]]*done[[:space:]]*$/ { exit }
    ' "$INST"
}

extract 1 > "$TMP/backup.sh"
extract 2 > "$TMP/restore.sh"

if [ -s "$TMP/backup.sh" ] && [ -s "$TMP/restore.sh" ]; then
    ok "обе половины найдены в install.sh"
else
    no "обе половины найдены" "бэкап и возврат" \
       "бэкап=$([ -s "$TMP/backup.sh" ] && echo да || echo нет) возврат=$([ -s "$TMP/restore.sh" ] && echo да || echo нет)"
    printf '\nPASSED: %d\nFAILED: %d\n' "$PASS" "$FAIL"
    exit 1
fi

# Заглушки окружения установщика.
cat > "$TMP/stubs.sh" <<'STUBS'
print_info()    { printf 'INFO: %s\n' "$*"; }
print_warning() { printf 'WARN: %s\n' "$*"; }
print_success() { printf 'OK: %s\n' "$*"; }
die()           { printf 'DIE: %s\n' "$*"; exit 7; }
STUBS

# --- 1. Полный цикл: бэкап → снос дерева → возврат ---------------------------
TREE="$TMP/opt/zapret2"
BK="$TMP/backup"
mkdir -p "$TREE/lists"
printf 'meduza.io\nrutracker.org\n' > "$TREE/lists/autohostlist-domains.txt"
printf 'example.blocked\n' > "$TREE/lists/discovered-domains.txt"
mkdir -p "$BK"

(
    . "$TMP/stubs.sh"
    ZAPRET2_DIR="$TREE"; backup_tmp="$BK"
    . "$TMP/backup.sh"
) > "$TMP/backup.log" 2>&1
_rc=$?

if [ "$_rc" = "0" ] && [ -f "$BK/autohostlist-domains.txt" ] && [ ! -e "$BK/discovered-domains.txt" ]; then
    ok "автохостлист сохраняется, retired discovery не сохраняется"
else
    no "состав бэкапа" "только autohostlist, rc=0" "rc=$_rc $(ls "$BK" 2>/dev/null | tr '\n' ' ')"
fi

# Even a stale backup must not resurrect the retired list.
printf 'example.blocked\n' > "$BK/discovered-domains.txt"
rm -rf "$TREE"
mkdir -p "$TREE/lists"

(
    . "$TMP/stubs.sh"
    ZAPRET2_DIR="$TREE"; backup_tmp="$BK"
    . "$TMP/restore.sh"
) > "$TMP/restore.log" 2>&1
_rc=$?

_got_auto=$(cat "$TREE/lists/autohostlist-domains.txt" 2>/dev/null)
_got_disc=$(cat "$TREE/lists/discovered-domains.txt" 2>/dev/null)

if [ "$_got_auto" = "$(printf 'meduza.io\nrutracker.org')" ]; then
    ok "autohostlist-domains.txt вернулся целиком"
else
    no "autohostlist-domains.txt вернулся" "meduza.io+rutracker.org" "[$_got_auto] rc=$_rc"
fi

if [ ! -e "$TREE/lists/discovered-domains.txt" ]; then
    ok "discovery не возвращается из старого бэкапа"
else
    no "discovery не возвращается" "absent" "$_got_disc"
fi

# --- 3. Бэкап fail-closed ----------------------------------------------------
#
# Копия не удалась (каталог бэкапа недоступен на запись) — установка обязана
# прерваться, потому что дальше по коду рабочее дерево уносится в .old и
# удаляется. Молчаливое продолжение здесь стоит человеку всех находок.
TREE2="$TMP/opt2/zapret2"; BK2="$TMP/backup2"
mkdir -p "$TREE2/lists" "$BK2"
printf 'meduza.io\n' > "$TREE2/lists/autohostlist-domains.txt"
chmod 500 "$BK2"

if [ "$(id -u)" = "0" ]; then
    printf '[SKIP] fail-closed: под root права на каталог не запрещают запись\n'
else
    (
        . "$TMP/stubs.sh"
        ZAPRET2_DIR="$TREE2"; backup_tmp="$BK2"
        . "$TMP/backup.sh"
    ) > "$TMP/failclosed.log" 2>&1
    _rc=$?
    if [ "$_rc" = "7" ] && grep -q '^DIE:' "$TMP/failclosed.log"; then
        ok "неудачный бэкап прерывает установку (die)"
    else
        no "бэкап fail-closed" "die, rc=7" "rc=$_rc $(head -1 "$TMP/failclosed.log" 2>/dev/null)"
    fi
fi
chmod 700 "$BK2" 2>/dev/null

# --- 4. Спасение из недостроенной установки знает про оба файла --------------
_rescue=$(awk '/Прошлая установка не завершилась/{c=1} c{print} c&&/^            done$/{exit}' "$INST")
_miss=""
# shellcheck disable=SC2043
for f in lists/autohostlist-domains.txt; do
    printf '%s' "$_rescue" | grep -q "$f" || _miss="$_miss $f"
done
if [ -z "$_miss" ]; then
    ok "спасение из .old переносит автохостлист"
else
    no "спасение из .old" "оба файла в списке" "нет:$_miss"
fi

# Execute the real migration, including a running legacy writer and a manual probe.
. "$ROOT/lib/config_official.sh"
ZAPRET2_DIR="$TREE"
Z2K_DISCOVERY_PROC_ROOT="$TMP/proc"
mkdir -p "$TMP/opt/etc/init.d" "$Z2K_DISCOVERY_PROC_ROOT/321" "$Z2K_DISCOVERY_PROC_ROOT/322"
printf '%s\000run\000' "$TMP/opt/sbin/z2k-detect" > "$Z2K_DISCOVERY_PROC_ROOT/321/cmdline"
printf '%s\000probe\000www.google.com\000' "$TMP/opt/sbin/z2k-detect" > "$Z2K_DISCOVERY_PROC_ROOT/322/cmdline"
printf 'www.google.com\n' > "$TREE/lists/discovered-domains.txt"
printf 'www.google.com\n' > "$TREE/lists/extra-domains.txt"
printf 'google.com\n' > "$TREE/lists/whitelist.txt"
touch "$TMP/opt/etc/init.d/S98z2k-detect" "$TREE/z2k-detect-watchdog.sh"
# Only the process-control boundary is mocked; migration logic is production code.
kill() { echo "$1" >> "$TMP/killed"; rm -rf "${Z2K_DISCOVERY_PROC_ROOT:?}/${1:?}"; }
z2k_retire_discovery
_rc=$?
if [ "$_rc" = 0 ] && [ ! -e "$TREE/lists/discovered-domains.txt" ] &&
   [ ! -e "$TMP/opt/etc/init.d/S98z2k-detect" ] && [ ! -e "$TREE/z2k-detect-watchdog.sh" ]; then
    ok "миграция удаляет публикацию и оба пути автозапуска"
else
    no "миграция" "deleted, rc=0" "rc=$_rc"
fi
if [ "$(cat "$TMP/killed")" = 321 ] && [ -f "$Z2K_DISCOVERY_PROC_ROOT/322/cmdline" ]; then
    ok "остановлен только run, ручная проба не тронута"
else
    no "выбор процесса" "321 only" "$(cat "$TMP/killed")"
fi
if [ "$(cat "$TREE/lists/extra-domains.txt")" = www.google.com ] &&
   [ "$(cat "$TREE/lists/whitelist.txt")" = google.com ] &&
   [ -s "$TREE/lists/autohostlist-domains.txt" ]; then
    ok "ручные списки и отдельный автохостлист сохранены"
else
    no "чужие списки" "preserved" "changed"
fi
z2k_retire_discovery && ok "повторная миграция идемпотентна" || no "повтор" 0 "$?"

# A still-running writer must veto deleting its publication.
mkdir -p "$Z2K_DISCOVERY_PROC_ROOT/321"
printf '%s\000run\000' "$TMP/opt/sbin/z2k-detect" > "$Z2K_DISCOVERY_PROC_ROOT/321/cmdline"
printf 'www.google.com\n' > "$TREE/lists/discovered-domains.txt"
kill() { return 1; }
if z2k_retire_discovery; then
    no "отказ остановки виден вызывающему" "nonzero" 0
else
    ok "отказ остановки виден вызывающему"
fi
[ -s "$TREE/lists/discovered-domains.txt" ] && ok "при живом writer публикация не удаляется" || no "writer" "preserved" "deleted"

printf '\nPASSED: %d\nFAILED: %d\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
