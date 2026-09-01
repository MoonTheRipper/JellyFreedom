# 04 — The VPN problem

**Read this before designing anything.** Everything else in the port is ordinary work. This
is the part where getting it wrong leaks the user's home IP to a torrent swarm, silently,
and looks fine until someone checks.

## What Linux does, and why it cannot be copied

A network namespace is a second, independent network stack. On Linux:

- `vpntorrent` contains WireGuard and **nothing else that reaches the internet**;
- TorrServer runs *inside* it, so it has no other route by construction — not by policy;
- `iptables -P OUTPUT DROP` inside the namespace is a second, independent lock;
- the orchestrator stays outside, on the LAN, and reaches in over a veth pair.

The property that matters is: **TorrServer cannot send a packet outside the tunnel, because
there is no path for one.** If WireGuard stops, its traffic does not fall back to the
default route — it fails with "Network is unreachable".

**Windows has no equivalent.** There are no network namespaces. Per-process routing policy
does not exist natively. Windows Firewall can filter by program and by *interface type*, but
not "this program may only use this interface". A per-app tunnel on Windows is normally done
with a WFP callout driver, which is a signed kernel driver — far outside this project.

## The options, honestly

### A. Machine-wide WireGuard tunnel + WFP kill switch — **recommended**

Use the official WireGuard for Windows service with `AllowedIPs = 0.0.0.0/0, ::/0` and its
built-in **"Block untunneled traffic (kill-switch)"**, which installs WFP filters that drop
anything not going through the tunnel, while permitting the local subnet.

- **Fail-closed:** yes, and enforced in the kernel by WFP, not by a route table.
- **LAN unaffected:** the local subnet is excluded, so Jellyfin↔Apple TV keeps working. This
  is the one thing to verify first, because if it were not true the whole design collapses.
- **Cost:** the orchestrator's own traffic (TMDB, Prowlarr, the dashboard's outbound) also
  goes through the tunnel. On Linux those deliberately stay outside — but that separation was
  a *consequence* of having namespaces, not a requirement. Metadata lookups over the VPN are
  fine, and arguably better for privacy.
- **Risk to name:** if the provider's endpoint is unreachable at boot, the machine has no
  internet at all until the tunnel comes up or the user disables the kill switch. Document
  that loudly, and make the dashboard say it in words.

**Why this one:** it is the only option that gets a real kernel-enforced kill switch without
a VM, a driver, or a second OS. It matches constraint 1 (no Docker, native binaries), and it
keeps the number of moving parts close to the Linux design.

**What would change my mind:** if the user needs the orchestrator to reach the LAN-side
services *and* the internet outside the tunnel simultaneously in a way WFP's LAN exclusion
does not cover. Test that before committing.

### B. WSL2 running the existing Linux stack

Run the current, proven `vpntorrent` namespace + TorrServer inside WSL2, with the
orchestrator native on Windows talking to it over the WSL network.

- **Pro:** the hard part is already written, audited and running.
- **Con:** WSL2 is a second OS and a heavyweight dependency; its NAT networking makes the
  host↔guest addressing fiddly (the veth model does not map cleanly); it does not start as a
  service without work; and it is arguably against the spirit of constraint 1.
- **Use it if** option A's kill switch cannot be made to coexist with LAN access.

### C. Hyper-V VM with the Linux build

Exact, and heaviest. A fallback if A and B both fail, not a first choice.

### D. Anything involving a custom WFP callout driver

Out of scope. Signed kernel drivers are a different project.

## Consequences for the code if you take option A

The SOCKS proxy (`internal/netnsproxy`) exists **only** because the orchestrator lives
outside the namespace and needs a way to egress through the tunnel. With a machine-wide
tunnel, it has no job — all traffic already goes through WireGuard.

That removes a component, but it also removes a **safety property**, and this is the subtle
part. Today, web-source extraction and thumbnail fetching are fail-closed *because dialling
the proxy fails when the proxy is down*. `internal/websource` refuses to run at all with no
proxy configured — deliberately, so there is no "direct" fallback.

If you delete the proxy, you must replace that guarantee, not just drop it:

> **Before any outbound fetch, assert the tunnel is up and is the active default route.**
> If it is not, refuse — do not fall back.

Wire that into the same place the watchdog already checks anonymity (see below). A boolean
"is the tunnel healthy" that everything consults is fine; silently proceeding is not.

## The watchdog

`vpntorrent/watchdog.sh` runs every 60s and does more than ping. It asserts **anonymity**,
not just reachability:

- the namespace's exit IP is fetched and compared against the host's public IP;
- if they match, the tunnel is not carrying the traffic and it takes the tunnel down;
- it re-establishes and re-checks rather than assuming.

Port this behaviour, not just the schedule. An earlier version used `curl -o /dev/null`,
which discarded the very body that reports the egress IP — it was checking that *something*
answered, not that the answer was the VPN. That is exactly the class of test that passes
while the property it is meant to guarantee is false.

## Definition of done for this component

Do not consider the VPN work finished until all four hold:

1. With the tunnel **up**, TorrServer's exit IP differs from the host's public IP.
2. With the tunnel **down**, TorrServer cannot reach any external address at all — verified
   by actually stopping the tunnel and observing the failure, not by reading firewall rules.
3. Jellyfin remains reachable from another device on the LAN in both states.
4. The dashboard reports the true state, and says plainly what to do when it is red.
