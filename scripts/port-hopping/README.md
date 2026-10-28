# Port hopping redirect rules

hy2tun does not install or remove firewall rules. Port hopping uses one HY2
listen port on the server plus an external NAT redirect that sends a UDP range
to that port. This directory contains deployment notes only; no script here
changes the firewall.

## Prerequisites

- A Linux server with nftables or iptables/ip6tables.
- root or `CAP_NET_ADMIN`.
- The HY2 `listen` port. The examples use `24443`; replace it with your own
  value. The server still listens on a single port.
- A UDP hop range outside `ports.auto_range` and every explicit UDP mapping.
  The examples use `30000-40000`, which is outside the default
  `auto_range: "20000-29999"`.
- The cloud security group allows the whole UDP range to the instance, because
  it filters before the host's NAT. The host INPUT chain only needs the final
  listen port, because the redirect happens in PREROUTING.
- IPv4 and IPv6 are handled separately. Add both rules if clients may use both
  protocols.

## nftables

Create the table, chain, and rule incrementally:

```bash
nft add table inet hy2tun_porthop
nft add chain inet hy2tun_porthop prerouting \
  '{ type nat hook prerouting priority dstnat; policy accept; }'
nft add rule inet hy2tun_porthop prerouting \
  udp dport 30000-40000 counter redirect to :24443
```

Or load the complete table from a file:

```nft
table inet hy2tun_porthop {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    udp dport 30000-40000 counter redirect to :24443
  }
}
```

```bash
nft -f /etc/nftables.d/hy2tun-porthop.nft
```

Use either the incremental commands or the file, not both. `nft add table` and
`nft add chain` are idempotent: re-running them succeeds and leaves the existing
object unchanged. `nft add rule` and reloading the file are not: each run
appends another rule, so duplicates accumulate silently. Use `nft create`
instead of `nft add` if an existing table or chain should be an error; `create`
returns "File exists". The `counter` statement is optional, but it makes the
verification step below possible; remove it if you do not want counters. Delete
the table before recreating it from scratch.

List and delete:

```bash
nft list table inet hy2tun_porthop
nft list ruleset
nft delete table inet hy2tun_porthop
```

To delete one rule instead of the whole table, list rules with handles:

```bash
nft -a list table inet hy2tun_porthop
nft delete rule inet hy2tun_porthop prerouting handle <handle>
```

## iptables and ip6tables

Add, list, and delete:

```bash
iptables -t nat -A PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
ip6tables -t nat -A PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443

iptables -t nat -L PREROUTING -n -v --line-numbers
ip6tables -t nat -L PREROUTING -n -v --line-numbers

iptables -t nat -D PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
ip6tables -t nat -D PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
```

The `-v` output shows packet counters. Use nftables or iptables, not both; a
duplicate redirect is harmless but makes troubleshooting harder.

If `listen` is bound to a specific address instead of `:port`, `REDIRECT` may
not reach it. Use DNAT to that address and port instead (replace `192.0.2.10`
with the server's address), for example:

```bash
iptables -t nat -A PREROUTING -p udp --dport 30000:40000 \
  -j DNAT --to-destination 192.0.2.10:24443
```

## Persistence

Rules added with `nft add` or `iptables -A` are lost on reboot. Persist them
with the distribution's mechanism and verify the rule after a reboot before
starting clients.

**nftables (Debian/Ubuntu):** add the table to `/etc/nftables.conf` or to a
file included by it, then enable the service:

```bash
systemctl enable --now nftables
systemctl status nftables
```

**nftables (dedicated systemd unit):** create
`/etc/systemd/system/hy2tun-porthop.service`:

```ini
[Unit]
Description=hy2tun port-hopping redirect
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/nftables.d/hy2tun-porthop.nft
ExecStop=-/usr/sbin/nft delete table inet hy2tun_porthop

[Install]
WantedBy=multi-user.target
```

Then run:

```bash
systemctl daemon-reload
systemctl enable --now hy2tun-porthop
```

**iptables (Debian/Ubuntu):** install `iptables-persistent` and save:

```bash
netfilter-persistent save
```

The rules are written to `/etc/iptables/rules.v4` and
`/etc/iptables/rules.v6`. Re-run `netfilter-persistent save` after every
change.

**iptables (RHEL/Fedora):** install `iptables-services`, save, and enable:

```bash
iptables-save > /etc/sysconfig/iptables
ip6tables-save > /etc/sysconfig/ip6tables
systemctl enable iptables ip6tables
```

**firewalld:** use permanent direct rules and reload:

```bash
firewall-cmd --permanent --direct --add-rule ipv4 nat PREROUTING 0 \
  -p udp --dport 30000:40000 -j REDIRECT --to-ports 24443
firewall-cmd --permanent --direct --add-rule ipv6 nat PREROUTING 0 \
  -p udp --dport 30000:40000 -j REDIRECT --to-ports 24443
firewall-cmd --reload
```

## Port conflicts

The hop range must not overlap UDP ports the server uses for reverse mappings:

- explicit `type: udp` mappings, including `remote_port: 0` allocations;
- `ports.allow` entries that cover UDP ports;
- `ports.auto_range`, whose default is `20000-29999`.

With the default `auto_range`, do not redirect `20000-30000`; use
`30000-40000` instead. A UDP mapping inside the redirected range would receive
HY2 traffic instead of its own datagrams and stop working. TCP mapping ports are
independent of the UDP redirect, but keeping ranges separate avoids confusion.

This release does not reserve the range automatically and does not manage
firewall rules. Server-side `listen: ":range"` support is planned for a later
release.

## Verification checklist

1. **Rule present.** `nft list table inet hy2tun_porthop` (with `counter`) or
   `iptables -t nat -L PREROUTING -n -v` shows the redirect. Send traffic and
   confirm the packet counter increases.
2. **Client warning.** Start the client with a multi-port `server`. The log
   contains:

   ```text
   port hopping enabled; server must redirect the entire UDP range to the HY2 listen port
   ```

   A missing warning means either `server` is a single port or `log.level` /
   `log.file` hides the warning; check both `server` and `log.level`.
3. **Conntrack.** On the server, run:

   ```bash
   conntrack -L -p udp | grep 'sport=24443'
   ```

   The reply tuple's source port is the listen port; the original destination
   port is from the hop range. Over several intervals, more than one original
   destination port should appear. Install `conntrack-tools` if the command is
   missing.
4. **Packet capture.** On the server, run:

   ```bash
   tcpdump -ni any 'udp and dst portrange 30000-40000'
   ```

   Packets arrive with a hop port as the destination because tcpdump sees them
   before NAT.
5. **Traffic test.** Keep a TCP mapping and a UDP mapping busy across several
   hop intervals. Both must continue to work; only the HY2 connection changes
   ports.
6. **Negative test.** Remove the redirect and confirm the client eventually
   fails after moving to an unredirected port. Restore the rule and confirm
   recovery.
7. **IPv6.** Repeat the conntrack or capture check with an IPv6 client when
   using an `inet` nftables table or the `ip6tables` rule.

`udphop` chooses a random port on every interval and may select the current
port again, so a single interval without a change is normal. Verify at least
one change across several intervals.

## Troubleshooting

- **No packets arrive.** Check the cloud security group, the host firewall, and
  that the rule is in the `nat` PREROUTING hook.
- **Packets arrive but the client times out.** The redirect target does not
  match the HY2 `listen` port, or the host INPUT chain blocks that port.
- **Some hop ports work and others do not.** An upstream firewall or security
  group allows only part of the range.
- **IPv4 works but IPv6 does not.** The IPv6 rule is missing, or the security
  group does not allow IPv6.
- **A UDP reverse mapping broke.** Its port is inside the hop range. Move the
  mapping or choose a different range.
- **`nft -f` reports "File exists".** The file uses `create table` or `create
  chain` for an object that already exists. The documented file uses `table`
  and does not produce this error; loading it again appends a duplicate rule
  instead. Check `nft list table inet hy2tun_porthop` for duplicates and delete
  the table before reloading if needed.
