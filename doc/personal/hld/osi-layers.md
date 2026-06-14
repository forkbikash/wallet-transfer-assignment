# OSI Layers and the Modern Approach

The **OSI model** (Open Systems Interconnection) is a 7-layer conceptual model for how data moves across a network. Each layer provides a service to the layer above it and consumes the service of the layer below it.

It answers the question:

> "When my request leaves the application and travels across the wire, what transformations happen — and who is responsible for each?"

## The 7 layers

```
 ┌───┬──────────────┬────────────────────────────┬─────────────────────┐
 │ # │ Layer        │ Responsibility             │ Examples            │
 ├───┼──────────────┼────────────────────────────┼─────────────────────┤
 │ 7 │ Application  │ App-level protocols        │ HTTP, gRPC, DNS,    │
 │   │              │                            │ Kafka protocol, SMTP│
 │ 6 │ Presentation │ Encoding, serialization,   │ TLS*, JSON, protobuf│
 │   │              │ encryption, compression    │ gzip                │
 │ 5 │ Session      │ Dialog control, sessions   │ RPC sessions,       │
 │   │              │                            │ NetBIOS             │
 │ 4 │ Transport    │ End-to-end delivery,       │ TCP, UDP, QUIC*     │
 │   │              │ ports, reliability         │                     │
 │ 3 │ Network      │ Routing between networks,  │ IP, ICMP, BGP       │
 │   │              │ logical addressing         │                     │
 │ 2 │ Data Link    │ Hop-to-hop framing,        │ Ethernet, ARP, MAC, │
 │   │              │ physical addressing        │ Wi-Fi (802.11)      │
 │ 1 │ Physical     │ Bits on the medium         │ Cables, radio,      │
 │   │              │                            │ fiber, voltages     │
 └───┴──────────────┴────────────────────────────┴─────────────────────┘
```

Mnemonic (bottom-up): **P**lease **D**o **N**ot **T**hrow **S**ausage **P**izza **A**way.

\* TLS and QUIC don't fit cleanly into one layer — see "where the model leaks" below.

### Encapsulation: how layers stack on the wire

Each layer wraps the payload from the layer above with its own header:

```
 App data:           [ HTTP request ]
 Transport (L4):     [ TCP hdr | HTTP request ]            = segment
 Network  (L3):      [ IP hdr  | TCP hdr | HTTP request ]  = packet
 Data Link (L2):     [ Eth hdr | IP | TCP | HTTP | FCS ]   = frame
 Physical (L1):      010110100111000...                    = bits
```

Receiving host peels the headers off in reverse order. Routers look at L3 (IP), switches look at L2 (MAC), firewalls typically look at L3/L4, load balancers at L4 or L7.

### Key addressing concepts per layer

- **L2** — MAC address: identifies a NIC on the *local* segment. Rewritten at every hop.
- **L3** — IP address: identifies a host across networks. Stable end-to-end (modulo NAT).
- **L4** — Port: identifies a *process/service* on the host (`:443`, `:5432`, `:9092`).
- **L7** — URLs, hostnames (via `Host`/SNI), methods, headers, cookies.

## The modern approach

### 1. Nobody implements OSI — everyone implements TCP/IP

OSI was a real protocol suite in the 1980s that lost to TCP/IP. What survived is the *vocabulary*. The model the internet actually runs on is the 4-layer **TCP/IP model**:

```
   OSI (conceptual)              TCP/IP (reality)
 ┌──────────────────┐         ┌──────────────────┐
 │ 7  Application   │         │                  │
 │ 6  Presentation  │  ───►   │   Application    │
 │ 5  Session       │         │                  │
 ├──────────────────┤         ├──────────────────┤
 │ 4  Transport     │  ───►   │   Transport      │
 ├──────────────────┤         ├──────────────────┤
 │ 3  Network       │  ───►   │   Internet       │
 ├──────────────────┤         ├──────────────────┤
 │ 2  Data Link     │  ───►   │   Link / Network │
 │ 1  Physical      │         │   Access         │
 └──────────────────┘         └──────────────────┘
```

Layers 5–6 effectively don't exist as separate protocols anymore; their jobs (sessions, serialization, encryption) are done inside the application layer or by libraries (TLS, protobuf, gzip).

In practice, engineers use OSI numbers as **shorthand**: "L4 load balancer", "L7 proxy", "L3 routing", "L2 switch". That's the model's real modern job — a shared vocabulary, not a blueprint.

### 2. Where the model leaks

Modern protocols deliberately blur the layers:

- **TLS** — sits "between" L4 and L7. Encryption is an L6 concern, but the handshake carries L7-ish data (SNI hostname, ALPN protocol negotiation).
- **QUIC** — a transport (L4) protocol, but built *on top of* UDP and with TLS 1.3 baked in. HTTP/3 = HTTP over QUIC.
- **VXLAN / overlay networks** — wraps L2 frames inside L4 UDP packets, so "lower" layers ride on top of "higher" ones. This is how Kubernetes CNIs and cloud VPCs build virtual networks.
- **NAT and middleboxes** — routers (supposedly L3 devices) rewriting L4 ports.
- **Service meshes (Envoy, Istio)** — sidecars that terminate TCP and TLS and make routing decisions on L7 data (HTTP paths, gRPC methods), effectively moving "networking" into userspace.
- **eBPF** — programs in the kernel that can inspect and redirect traffic across L2–L7 boundaries (e.g. Cilium replacing kube-proxy).

### 3. Practical layer map for backend engineers

| You're dealing with...                    | Layer | Typical tools                   |
|-------------------------------------------|-------|---------------------------------|
| DNS resolution failures                    | L7    | `dig`, `nslookup`               |
| HTTP status codes, routing by path/host    | L7    | ALB, nginx, Envoy               |
| TLS cert / handshake errors                | L6-ish| `openssl s_client`              |
| Connection refused / timeout, port issues  | L4    | `nc`, `telnet`, `ss`, NLB       |
| "Host unreachable", routing, subnets, CIDR | L3    | `ping`, `traceroute`, `ip route`|
| ARP problems, VLANs, MTU mismatches        | L2    | `arp`, `tcpdump -e`             |
| Bad cable / flaky NIC / Wi-Fi signal       | L1    | swap the cable 🙂               |

Debugging heuristic: **walk the stack bottom-up** — is the link up (L1/L2)? can I reach the IP (L3)? can I open the port (L4)? does TLS handshake (L6)? does the request succeed (L7)?

## Which OSI layer do AWS Security Groups work on?

**Layers 3 and 4** (Network and Transport).

A security group is a **stateful virtual firewall** attached to an ENI (elastic network interface). Its rules can only match on:

- **Protocol** — TCP / UDP / ICMP (L3/L4)
- **Port range** — e.g. `443`, `5432`, `9092` (L4)
- **Source / destination** — CIDR block, IP, or another security group ID (L3)

```
Inbound rule:  allow  TCP  port 9092  from sg-0abc123 (the app SG)
                ▲       ▲      ▲            ▲
                │      L4     L4           L3 (resolved to member IPs)
              action
```

It **cannot** see anything at L7: no URLs, no HTTP methods, no headers, no hostnames, no SQL. If you need to filter "block requests to `/admin`" or "rate-limit by header", that's **AWS WAF** (L7), not a security group.

Key properties worth remembering (common interview follow-ups):

- **Stateful** — if inbound traffic is allowed, the response is automatically allowed out; no need for a matching outbound rule. (This connection tracking is an L4 behavior.)
- **Allow rules only** — you cannot write deny rules. Anything not explicitly allowed is denied.
- **Contrast with NACLs** — Network ACLs also operate at L3/L4 but are *stateless* (must allow return traffic explicitly), support *deny* rules, are evaluated in *rule-number order*, and attach to *subnets* rather than ENIs.

```
            ┌─────────────── L7  AWS WAF (paths, headers, rate limits)
            │
 Request ──►│  ┌──────────── L3/L4  NACL (subnet boundary, stateless)
            │  │
            │  │  ┌───────── L3/L4  Security Group (ENI, stateful)
            ▼  ▼  ▼
           [ EC2 / RDS / MSK ... ]
```

## TL;DR

- OSI is a 7-layer **conceptual** model; the internet actually runs the 4-layer TCP/IP stack. OSI survives as shared vocabulary: L2 switch, L3 route, L4 load balancer, L7 proxy.
- Layers 5–6 dissolved into libraries (TLS, protobuf); modern protocols like QUIC and overlay networks intentionally blur the boundaries.
- Debug network issues bottom-up: link → IP → port → TLS → application.
- **AWS Security Groups operate at L3/L4** — stateful, allow-only, matching on protocol/port/IP. For L7 filtering use WAF; for stateless subnet-level rules use NACLs.
