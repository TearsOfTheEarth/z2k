#!/bin/sh
# Existing VPN rules must disappear on an additive repair, without touching
# another queue, the ISP or an explicitly selected VPN.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT HUP INT TERM
. "$ROOT/lib/wan.sh"
awk '/^z2k_sweep_auto_vpn_nfqueue\(\)/{f=1} f{print} f && /^}/{exit}' "$ROOT/files/S99zapret2.new" > "$TMP/fn"
. "$TMP/fn"
mkdir "$TMP/bin"
export TMP
cat > "$TMP/bin/iptables-save" <<'SAVE'
#!/bin/sh
cat <<'RULES'
-A POSTROUTING -o nwg0 -p tcp -j NFQUEUE --queue-num 200 --queue-bypass
-A INPUT -i tun0 -p udp -j NFQUEUE --queue-num 200 --queue-bypass
-A FORWARD -i nwg0 -p tcp -j NFQUEUE --queue-num 2000 --queue-bypass
-A POSTROUTING -o eth3 -p tcp -j NFQUEUE --queue-num 200 --queue-bypass
-A POSTROUTING -o usb0 -p tcp -j NFQUEUE --queue-num 200 --queue-bypass
RULES
SAVE
cat > "$TMP/bin/iptables" <<'DELETE'
#!/bin/sh
printf '%s\n' "$*" >> "$TMP/deleted"
DELETE
cp "$TMP/bin/iptables-save" "$TMP/bin/ip6tables-save"
cp "$TMP/bin/iptables" "$TMP/bin/ip6tables"
chmod +x "$TMP/bin/"*
PATH="$TMP/bin:$PATH"; export PATH
QNUM=200
z2k_sweep_auto_vpn_nfqueue
[ "$(wc -l < "$TMP/deleted" | tr -d ' ')" = 4 ]
! grep -Eq 'queue-num 2000|eth3|usb0' "$TMP/deleted"
: > "$TMP/deleted"
WAN_IFACE='eth3,nwg0 tun0'
z2k_sweep_auto_vpn_nfqueue
[ ! -s "$TMP/deleted" ]
printf 'PASS: automatic VPN queue cleanup preserves ISP, other queue and explicit override\n'

# Full stop used during rollback must also match the queue number exactly.
awk '/^z2k_sweep_orphan_nfqueue\(\)/{f=1} f{print} f && /^}/{exit}' "$ROOT/files/S99zapret2.new" > "$TMP/orphan"
. "$TMP/orphan"
: > "$TMP/deleted"
z2k_sweep_orphan_nfqueue
[ "$(wc -l < "$TMP/deleted" | tr -d ' ')" = 8 ]
! grep -q 'queue-num 2000' "$TMP/deleted"
printf 'PASS: full stop preserves another application queue with shared numeric prefix\n'
