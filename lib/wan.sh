#!/bin/sh
# Shared WAN discovery for the firewall and its periodic repair. Read-only:
# never change provider priorities, policy rules or the user's WAN_IFACE.
# Only automatic discovery is filtered. WAN_IFACE remains an explicit opt-in
# for users who intentionally run desync through a tunnel. Do not query netlink:
# ip link can hang on an unhealthy native WireGuard driver (issue #18).
z2k_wan_auto_excluded() {
    local dev="$1" sys="${Z2K_NET_CLASS:-/sys/class/net}" kind
    case "$dev" in
        lo|br[0-9]*|z2ktg*|z2ktun*|nwg[0-9]*|wg[0-9]*|awg[0-9]*|tun[0-9]*|tap[0-9]*|vpn[0-9]*|gre[0-9]*|gretap[0-9]*|ip6gre[0-9]*|ip6tnl[0-9]*|sit[0-9]*|ipip[0-9]*|vti[0-9]*|vti6[0-9]*|xfrm[0-9]*) return 0 ;;
    esac
    [ ! -d "$sys/$dev/bridge" ] || return 0
    # tun_flags also identifies renamed TAP devices (Ethernet type 1).
    [ ! -e "$sys/$dev/tun_flags" ] || return 0
    kind=$(cat "$sys/$dev/type" 2>/dev/null) || kind=
    case "$kind" in
        768|769|772|776|778|823) return 0 ;;
        # ARPHRD_NONE is also used by some USB raw-IP modem drivers. Unlike
        # WireGuard/XFRM, those have a backing hardware device in sysfs.
        65534) [ -e "$sys/$dev/device" ] || return 0 ;;
    esac
    # Ethernet/VLAN/Wi-Fi/USB and PPP (including provider PPPoE) are retained.
    return 1
}

z2k_wan_ifaces() {
    local family="$1" override="${2:-}" routes candidates dev
    if [ -n "$override" ]; then
        printf '%s\n' "$override" | tr ',' ' ' | awk '{for(i=1;i<=NF;i++) if(!seen[$i]++) print $i}' | xargs
        return 0
    fi
    # Keenetic places secondary ISP defaults in policy tables, not necessarily
    # in main. Some old ip builds cannot dump all tables: retain main fallback.
    routes=$(ip "$family" route show table all 2>/dev/null) ||
        routes=$(ip "$family" route show default 2>/dev/null) || return 0
    candidates=$(printf '%s\n' "$routes" | awk '
        function emit(d,bad) {
            if(d!="" && !bad && !seen[d]++) print d
        }
        function devices( i,d,bad) {
            for(i=1;i<=NF;i++) {
                if($i=="nexthop") {emit(d,bad); d=""; bad=0}
                if($i=="dev") d=$(i+1)
                if($i=="dead" || $i=="linkdown") bad=1
            }
            emit(d,bad)
        }
        # iproute2 can put ECMP nexthops on continuation lines. Only retain
        # those belonging to a usable default; never a LAN or blackhole route.
        $1=="nexthop" {if(active) devices(); next}
        {
            active=($1=="default" || $1=="0.0.0.0/0" || $1=="::/0")
            if($0 ~ /(^|[[:space:]])table (988|989)([[:space:]]|$)/) active=0
            if(active) devices()
        }
    ')
    for dev in $candidates; do
        z2k_wan_auto_excluded "$dev" && continue
        printf '%s\n' "$dev"
    done | xargs
}
