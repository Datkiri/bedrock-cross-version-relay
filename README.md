# bedrock-cross-version-relay

**Cross-version relay / protocol translator for Minecraft Bedrock Edition LAN worlds.**

<sub>Maintained by **ILwHee** · MIT · contributions welcome · binary name: `xvrelay`</sub>

Let a **1.26.51 client join a 1.26.33 host**, and let a **1.26.33 client join a 1.26.51 host** —
without patching the game, without a modified APK, and without everyone installing the same version.

It works as a small Go proxy: the client connects to the relay using its own protocol, the relay
talks to the LAN world using the host's protocol, and packets are re-encoded and block palettes are
re-mapped in both directions.

```
   Minecraft client            this relay                   LAN world (host)
   version A  ──────────▶  listen :19164  ──────────▶  NetherNet / RakNet
              protocol A                protocol B
```

Keywords: `minecraft bedrock` · `cross-version` · `crossplay` · `protocol translation` · `protocol translator` ·
`LAN relay` · `NetherNet` · `RakNet` · `gophertunnel` · `block palette` · `palette remap` ·
`protocol 2193` / `2169` / `2168` / `1001` · `1.26.51` · `1.26.45` · `1.26.33` · `proxy` · `Go`

---

## Why

Minecraft Bedrock bumps its network protocol with almost every minor release, and clients refuse to
join a world that speaks a different one. On Java Edition the community solved this years ago with
proxy-side translation (ViaVersion and friends). On Bedrock the same idea is much less common,
because Bedrock is not a single protocol on top of TCP: it is RakNet or NetherNet for transport,
a compressed packet stream on top, and — the hard part — **versioned registries** (block palettes,
item IDs, recipes) that must agree between client and server for terrain and inventory to make sense.

This project is an experiment in doing exactly that, out in the open:

- translate the **packet stream** between two Bedrock protocols, both directions;
- translate **block palettes** in `SubChunk` (v8/v9), inline `LevelChunk`, `UpdateBlock`,
  `UpdateBlockSynced`, `UpdateSubChunkBlocks`, and the reverse direction for clicked/broken blocks;
- translate the **item registry** so inventories survive the hop — a table is pinned for each of
  `1001`, `2168`, `2169` and `2193`; see [Block and item translation](#block-and-item-translation);
- keep everything else as close to "just forward the bytes" as possible, so behaviour stays
  predictable and failures are visible in the log instead of silently corrupting the world.

## Supported versions

Protocol IDs, not marketing versions — a build can share a protocol ID and still differ in registries.

| Protocol | Minecraft versions | Role |
| --- | --- | --- |
| `1001` | 1.26.30 – 1.26.36 | client and host |
| `2168` | 1.26.40 – 1.26.44 | client and host |
| `2169` | 1.26.45 | client and host |
| `2193` | 1.26.50 – 1.26.52 | client and host (native) |

`1.26.52` shares protocol `2193` with `1.26.50`/`1.26.51`, but the *version string* is not
interchangeable: a Bedrock client refuses a server whose `GameVersion` is older than its own
(measured: a 1.26.52 client handed `1.26.50` crashes itself after ~0.3 s), so the relay overrides
`StartGame.GameVersion` with the client's real string.

Any client protocol can be paired with any host protocol — 16 combinations. Protocol detection is
automatic by default: the relay reads the host's protocol from LAN discovery (`ServerData` v5/v6/v7),
and if the host broadcasts a format without a `Protocol` field it probes the host with
`2193 → 2169 → 1001 → 2168` (a wrong-protocol login is rejected almost immediately, so the probe is cheap).

## Quick start

Requirements: **Go 1.25+**, and a Bedrock world open to LAN on the same machine or network.
The NetherNet and gophertunnel forks the engine needs are vendored in `third_party/`, so the build
needs no extra checkouts.

Prebuilt binaries for six targets are attached to each release: `linux-amd64`, `linux-arm64`,
`linux-arm` (Raspberry Pi), `windows-amd64`, `darwin-amd64`, `darwin-arm64`. They are statically
linked, need no runtime, and are the same bytes `make release` produces.

```bash
# Linux / macOS
./xvrelay-linux-amd64 -version

# Windows
xvrelay-windows-amd64.exe -version
```

Build from source instead:

```bash
go build -buildvcs=false -trimpath -o dist/xvrelay ./cmd/xvrelay
```

Then, with the world already open to LAN:

```bash
# listen on 0.0.0.0:19164, auto-detect the world and both protocols
./dist/xvrelay

# pick a specific world when several are open (level name or NetworkID)
./dist/xvrelay -world "My World"

# force protocols instead of detecting them
./dist/xvrelay -client-proto 2193 -host-proto 1001

# BDS / direct-host lab: skip LAN discovery, dial the server over RakNet
# (requires server.properties: transport=raknet)
./dist/xvrelay -host-raknet 127.0.0.1:19132 -host-proto 1001 -client-proto auto -listen 127.0.0.1:19164

# CONN-002: advertise on LAN and accept clients over NetherNet/WebRTC instead of
# RakNet. -listen is ignored; clients find the relay in the LAN world list.
./dist/xvrelay -host-raknet 127.0.0.1:19132 -host-proto 1001 -listen-nethernet
```

Point the older/newer client at `<relay-ip>:19164` using **Add Server** (RakNet ingress, the
default). With `-listen-nethernet` the relay instead appears as a LAN world on UDP 7551 and is
joined through NetherNet; see "NetherNet ingress (CONN-002)". The relay logs both directions;
`-trace 40` (default) prints the first 40 packet names per direction, `-trace 0` silences it.

For an automated check without a game client, `cmd/selfcheck` is a headless Bedrock client that
spawns through the relay and reports assertions as JSON:

```bash
go build -buildvcs=false -o dist/selfcheck ./cmd/selfcheck
./dist/selfcheck -addr 127.0.0.1:19164 -proto 2193 -soak 10s -json dist/selfcheck-2193.json -v

# CONN-002: join the relay the way a game client selects a LAN world — discover
# its world card on UDP 7551, then dial that network ID over NetherNet/WebRTC.
./dist/selfcheck -lan-join -proto 2193 -soak 10s -json dist/selfcheck-lan-2193.json -v

# exercise the C2H interaction path (place/click blocks, spawn eggs, hotbar):
./dist/selfcheck -addr 127.0.0.1:19164 -proto 1001 -interactions -soak 8s -json dist/selfcheck-interactions.json -v
```

Useful flags: `-proto` (`1001|2168|2169|2193`), `-reconnect N`, `-soak <duration>`,
`-spawn-timeout`, `-json <path>`, `-v`, `-interactions`. For `-lan-join`: `-lan-addr` (default
`127.0.0.1:7551`), `-network-id` (skip discovery), `-lan-timeout`. `-lan-join` requires the
relay to run with `-listen-nethernet`; `-addr` is ignored. It exits non-zero when any assertion
fails, so it can be wired into CI. All four protocols have been live-tested against a BDS 1.26.33.1
host and against a real LAN world on another machine; see
[What is actually verified](#what-is-actually-verified).

Multiple clients of *different* versions can be relayed into the same host at once — spawn capture and
the spawn-prefix gate are per-session, keyed by connection.

## How it works

1. **Discovery** (`internal/lansignal`) binds UDP `7551` and listens for the host's LAN announcement
   packets, which carry the world's NetherNet ID and signalling address. A vanilla client *receives*
   these; it does not send them — that detail is why a naive joiner never sees a world.
2. **Two connections.** The client side is a `minecraft.Listener` speaking the client's protocol; the
   host side is a `Dialer` speaking the host's protocol. gophertunnel converts to/from its native
   protocol on read and write, so the relay translates rather than copies bytes.
3. **Spawn gating.** The host's `StartGame` and the following chunk burst are captured, translated and
   replayed to the client in the right order, with the client's protocol, before the client is allowed
   to see live traffic. This is the part that makes or breaks spawning.
4. **Terrain translation** (`internal/blocktranslate`). Block palettes are pinned, per-protocol
   snapshots (see below); every state in a chunk's palette is mapped by typed-state hash, never by
   ordinal position in a different version's palette.
5. **Steady state.** Packets that need no translation are forwarded as-is. Anything that fails to
   decode/encode closes the session with a log line — the relay never drops a packet and pretends
   the session is healthy.

### Block and item translation

- Canonical palette is the host's or the client's, depending on direction; the target is looked up by
  **typed state hash** (FNV-1a over the little-endian NBT name + states), so an ID that means "grass
  block" in 1.26.33 cannot silently become "grass block" the slab in 1.26.51.
- Vanilla state with no valid mapping on the target version becomes a **visible placeholder**
  (`minecraft:info_update`), counted in the log. It is never turned into air and never silently
  claimed as exact.
- A source ID that is not in the pinned palette at all is a **hard error**, not a guess.
- Item registries are per-protocol as well (`internal/blocktranslate/items.go`): `2193` comes from
  the pinned Cloudburst snapshot (2076 items); `1001` (1933) and `2169` (1976) are built from the
  same `go-multiversion` `vanilla_items.nbt` snapshots the protocol adapters use, via
  `mapping.NewItemMapper(...).TargetEntries()`. `2168` shares `2169` because
  `v1_26_44.NewWithRegistries` delegates to `v1_26_45`. A test asserts each table is byte-for-byte
  the name/ID mapping its registry-aware codec emits.
- Which table goes into `GameData.Items` depends on the client codec. A **registry-aware** codec
  (`1001`, `2168` and `2169` by default — all built with `NewWithRegistries`) translates
  `ItemRegistry` itself and expects the **native `2193`** table as input — feeding it the client's
  own table makes `ValidateNativeEntries` fail and the packet is dropped. A **registry-less** codec
  (the `2168`/`2169` plain `New()` fallback behind `-map-gameplay-items=false`, and the native
  `2193` default protocol) passes it through, so it must be handed the client's own table.
  `clientItemRegistry()` implements that rule; both branches are tested in `item_registry_test.go`
  and `item_gameplay_test.go`.
- The registry-aware codecs also translate item runtime IDs **inside gameplay packets**, in both
  directions, not just the advertised registry. Measured offline: 259 item names carry different
  `2193`/`2169` IDs (1717 are identical), `minecraft:apple` maps `882 → 878` and back `878 → 882`
  (`1001` uses `285`), and `plain New()` leaves `882` untouched. `item_gameplay_test.go` pins a
  **six-packet smoke subset** (including `ItemRegistry`); `semantic_coverage_test.go` measures the
  real coverage: **10 stack-carrying packet families (8 H2C + 2 C2H) + the `ItemRegistry` table** are remapped by
  `1001`/`2168`/`2169`, with `plain New()` and the native `2193` identity codec as pass-through
  controls. An unknown runtime ID is not dropped but replaced by AIR (`hideUnknown=true`).
  Disable with `-map-gameplay-items=false` to reproduce the old behaviour. See
  [Semantic packet coverage](#semantic-packet-coverage-proto-005).
- `SubChunk` v8 → v9 is upgraded when the target is `2193` (the Y byte after the storage count is new
  in v9); downgrades keep the remaining LevelChunk tail bytes intact.

Palette sources are pinned by commit and SHA-256 in
`internal/blocktranslate/data/manifest.json` (Apache-2.0, from [CloudburstMC/Data](https://github.com/CloudburstMC/Data)).
The historical item snapshots are pinned by the `go-multiversion` version in `go.sum`; their own
manifests record the dragonfly commits. Protocol `2169` shares the `2168` block schema; a test
asserts the full typed-state set matches the pinned `2169` snapshot rather than reusing the `2168`
codec.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `0.0.0.0:19164` | address the clients connect to (RakNet ingress; ignored with `-listen-nethernet`) |
| `-listen-nethernet` | off | `CONN-002`: accept clients over NetherNet/WebRTC LAN discovery instead of RakNet; the relay advertises on UDP 7551 through `lansignal` |
| `-world` | *(auto)* | level name or NetworkID; empty = the only world found |
| `-timeout` | `20s` | LAN discovery window |
| `-client-proto` | `auto` | force the client-side protocol (`1001`/`2168`/`2169`/`2193`) |
| `-host-proto` | `auto` | force the host-side protocol |
| `-host-raknet` | *(off)* | dial the host directly over RakNet (`host:port`), skipping LAN discovery — for a BDS with `transport=raknet` and the self-check harness |
| `-map-gameplay-items` | `true` | build the `2168`/`2169` codecs with `NewWithRegistries` so item IDs inside gameplay packets are translated; `false` restores plain `New()` for bisect |
| `-trace` | `40` | print the first N packet names per direction, `0` = off |
| `-dial-timeout` | `60s` | max time to dial the host, resource-pack download included |
| `-startgame` | `raw` | `raw` = replay the host's original bytes when the wire matches, `rebuild` = re-encode |
| `-identity` | `auto` | send the client's real name + XUID, or `bot` to present as Steve |
| `-noblocktranslate` | off | disable palette translation (keeps chunk framing) — debugging aid |
| `-verify` | off | encode→decode every client-bound packet and drop the broken ones (slower, diagnostic) |
| `-audit` | off | re-parse client-bound chunks and check state IDs against the target registry |
| `-probe` | off | dial the world once and exit, no client needed |
| `-pack-mode` | `skip` | `skip` / `download` / `limit` resource-pack policy (`CONN-005`) |
| `-pack-max-mb` | `8` | per-pack declared-size ceiling in `limit` mode, `0` = none |
| `-pack-budget-mb` | `32` | total accounted pack bytes per dial, `0` = unlimited |
| `-pack-overrun` | `abort` | `abort` cancels the dial when the REAL size breaks a ceiling; `report` only logs |
| `-pack-prefetch` | off | extra host dial before the listener; needs `-pack-mode download\|limit` |

There are also a number of `-strip*` / `-no*` bisect flags (`-stripregs`, `-stripitems`,
`-stripactors`, `-nochunks`, `-nosubchunk`, `-onlyprefix`, `-nopush`, `-dropids`, …). They exist to
isolate a failing packet by elimination, not for normal play. Note that "the client survives with `-X`
off" does **not** prove `X` was the cause — a client that receives no terrain cannot crash on terrain.

## What is actually verified

Honest scope, because "it builds" and "it works" are different claims.

- **Wire/model level:** 16 protocol pairs × 8 checks = **128 checks, all passing** after the fixes
  recorded in [`docs/xvrelay-protocol-matrix-2026-10-26.md`](docs/xvrelay-protocol-matrix-2026-10-26.md).
  The checks cover `StartGame` metadata/rules/experiments/permissions, inline `LevelChunk`,
  request-mode `LevelChunk`, `SubChunk` payload and offsets, host chat, the inventory envelope,
  client `SubChunkRequest`, and `UpdateBlock` codec→relay→codec round-tripping.
  `make matrix` prints the per-pair count and writes `dist/matrix.json`; the last run reported
  `checks=8` with no findings for all 16 pairs.
- **Registry level:** every block state in the three pinned palettes is checked for identity mapping,
  and every unsupported state is asserted to produce the visible placeholder. All four item tables
  are checked for unique names/IDs and for matching the registry-aware codecs' output; the
  pass-through codec case and the non-native-input drop are both asserted in
  `cmd/xvrelay/item_registry_test.go`.
- **Gameplay item level (offline):** `cmd/xvrelay/item_gameplay_test.go` asserts bidirectional
  runtime-ID translation for `1001`/`2168`/`2169`, a six-packet smoke subset is not dropped, plain
  `New()` is a pass-through control, unknown IDs become AIR, and `resolveProto()` really returns a
  registry-aware adapter. Measured: 259 differing IDs / 1717 identical IDs among the 1976 shared
  names; 2076 native entries → 1976 emitted. `cmd/xvrelay/semantic_coverage_test.go` then measures
  the **full** item-family coverage: 10 stack-carrying families mapped on all three registry-aware
  codecs, the `ItemRegistry` table emitting `1933`/`1976`/`1976` entries, plus controls.
- **Live wire item census:** `cmd/clientclone` decodes >5,000 item stacks per session from a live
  BDS 1.26.33.1 and resolves every ID against the client's own table; all four protocols reported
  `lost=0 unknown=0 ok=true` (see "Live item census" below). This is the measured replacement for
  the vague "no real client has looked at the inventory" caveat — stacks are decoded, not rendered.
  **Scope measured by `PROTO-005` / re-measured 2026-11-05:** `stacksFrom()` extracts all **8
  host→client** stack-carrying families; the other **2** of the codec's 10 families
  (`InventoryTransaction`, `PlayerAuthInput.ItemInteractionData`) are **client→host** and cannot be
  observed from a client at all. The one-player BDS lab emits only 5–6 of the 8 H2C families
  (`AddPlayer` / `MobArmourEquipment` never occur), so the >5,000 stacks remain a lower bound on
  the wire's item IDs — see "Live item census".
- **Invisible-item audit (offline, wire-level):** `cmd/itemaudit` pushes every native `2193` item
  through the real client codec, encodes the packet with that protocol's writer, then decodes it
  with its own packet pool/reader — the same path `Conn.ReadPacket` uses. `NetworkID=0` after the
  round trip means the client decodes AIR. Measured 2026-11-05: `1001` → **143** invisible of 2076,
  `2168`/`2169` → **100**, `2193` → **0**, `wrong_id=0` everywhere, and the lost set matches
  `MappingReport().ItemFallbacks` exactly (143 for `1001`, 100 for `2169`; `2168` does not export
  that report, so its wire measurement is the direct evidence). The missing items are the 16-colour
  `concrete_slab` / `concrete_double_slab` / `concrete_stairs` / `wool_slab` / `wool_double_slab` /
  `wool_stairs` families, plus `cushion`, the `poplar` wood set, `photo_item`, `portfolio`,
  `shelf_mushroom` and `straw_bed` on `1001`. `go run ./cmd/itemaudit -json dist/item-audit.json`
  writes the complete name/ID list; `cmd/itemaudit/main_test.go` pins the counts and the
  protocol-1001 `translatedPacket` wire trap. This is still **not** a real client rendering an
  inventory. The Cinnabar desktop app is GUI-based (`make play`), pinned to protocol 2225 and
  licensed PolyForm Noncommercial, so **no code from it is used here**; its Go core
  (`core/cmd/bedrock-core`, `core/proxy/local_bds_live_test.go`) served only as a behavioural
  reference for the separate MIT terminal client `cmd/clientclone` described below.
- **Live self-check (headless):** `cmd/selfcheck` is an independent terminal Bedrock client. It
  dials the relay's RakNet listener, spawns, counts 18 packet families, checks `GameData().Items`
  against the expected table, samples heap/goroutines/`num_gc`, supports `-reconnect` and
  `-soak`, writes a
  JSON report and exits non-zero on a failed assertion. Against a real BDS 1.26.33.1 host
  (`transport=raknet`, reached with `-host-raknet 127.0.0.1:19132 -host-proto 1001`) the final
  uniform run on the fixed binary (`-client-proto auto`, `-soak 4s`) gave all four protocols
  spawning with 921 LevelChunks, `registry=true`, `soak_complete`, `OK=true`, exit 0:
  `1001` 1239 packets / `items=1933`, `2168` 1217 / 1976, `2169` 1304 / 1976,
  `2193` 1654 / 2076. That is four live client→relay→BDS paths, still not all 16 protocol pairs.
- **Live command-driven client (headless):** `cmd/clientclone` is a terminal Bedrock client driven
  by one JSON command per line. It joins the relay (or a BDS directly), spawns, sends 20 Hz
  `PlayerAuthInput`, echoes latency, answers forms, requests subchunks, asserts packet counters and
  writes a JSON report. Against the same BDS 1.26.33.1 it found and then verified the fix for a
  listener-side `shieldID` bug: before the fix `1001` reported `decode_errors=2`,
  `bad_packets=2`, `command_failures=1`, `ok=false` and dropped `CreativeContent`/`CraftingData`
  with `EOF`; after the fix it reports `decode_errors=0`, `packets_total=1252` (was 882),
  `CreativeContent=2`, `CraftingData=1`, `ok=true`, exit 0. All four client protocols then passed
  (`1001` shield 387, `2168`/`2169`/`2193` shield 358; all `spawned=true`, `decode_errors=0`).
  `cmd/xvrelay/item_registry_test.go` pins the shield IDs, the `+8` byte `BlockingTick` delta and
  the round-trip oracle. This is a headless codec-level client, not a rendering game client.
- **Hot-path cost (offline microbenchmarks):** `make bench` measures `ns/op`, `B/op` and
  `allocs/op` for codec conversion, item mapping and chunk translation across all four protocol
  codecs. Measured 2026-11-05: a registry-aware 36-slot `InventoryContent` costs **2.4–2.8 µs**
  (identity `2193` = **15 ns**), a full 2076-item `CreativeContent` once per spawn **~100 µs /
  205 KB**, a ~2 KB sub-chunk rewrite **~1.2 µs / 5.5 KB**, and the per-session block mapper
  build **~660 µs / 609 KB** — which is why it is cached behind `sync.Once`. This is
  **per-operation cost, not process-level CPU or end-to-end latency**; see "Hot-path benchmarks".
- **Concurrency / lifecycle (`CONN-004`, measured 2026-11-05):** `cmd/xvrelay/concurrency_test.go`
  drives **8 real loopback RakNet sessions at once** over `sessions`, `clientGates`,
  `spawnCapture` and `clientGate` atomics, asserting each session resolves its own state, emits
  its own captured prelude bytes, keeps its own packet counter, and leaves both global maps back
  at baseline after cleanup. `CGO_ENABLED=1 go test -race -run TestConcurrent` is clean. Live on
  BDS 1.26.33.1 (`transport=raknet`, `-host-raknet 127.0.0.1:19132 -host-proto 1001`):
  `selfcheck -reconnect 3 -soak 4s` → **4/4 sessions `OK=true`** (1324 / 1217 / 1174 / 1142
  packets, 921 chunks each, `registry=true`, `soak_complete`); **3 concurrent clients** on
  `1001`/`2169`/`2193` → **3/3 `OK=true`**; the same run against a relay **built with `-race`**
  reported **0 `DATA RACE`**. A 25 s soak held goroutines at 5→7→6 with `num_gc` 4→7 then flat.
  Two honest boundaries: (1) `dialHost()` holds `hostProtoMu` for the whole host dial, so the
  three clients spawned ~9 s apart (17:41:59 / 17:42:08 / 17:42:17) — throughput is serialised,
  not racy; (2) `heap_alloc` still grows ~0.04 MB/s in steady state with GC count flat, so a
  small retained-heap trend is **not** ruled out without an hours-long soak.
- **Resource-pack prefetch / cost guard (`CONN-005`, measured 2026-11-05):** `cmd/xvrelay/packs.go`
  wraps the host `minecraft.Protocol` and observes `ResourcePacksInfo` / `ResourcePackDataInfo`
  in `ConvertToLatest` — i.e. **after decode, before** `handlePacket` — so the `DownloadResourcePack`
  callback can decide from real sizes instead of guessing. Ten unit tests (`packs_test.go`) pin the
  policy, budget, lie detection, overrun abort and skipped-pack isolation. Live on BDS 1.26.33.1
  with a minimal `.mcpack` (pack `9f1b0c2e-…`): the host advertised **594 B** but
  `ResourcePackDataInfo` reported **734 B** (`size_lies=1`), the guard reconciled the accounted cost
  to **734 B**, and `-pack-prefetch` stored **1 pack** in `7.97 s`; the relay then logged
  `serving the host's packs to the client count=1` and `selfcheck` spawned with `OK=true`
  (1161 packets, 921 chunks, `registry=true`). With `-pack-budget-mb 0.0006` (629 B, between the
  declared and real sizes), `-pack-overrun abort` cancelled the dial in **35 ms** with exit 1, while
  `-pack-overrun report` let the same dial succeed (`overruns=1`). Default `-pack-mode skip`
  downloaded **0** packs. See "Resource-pack prefetch".
- **Semantic packet coverage (`PROTO-005`, measured 2026-11-05):** `cmd/xvrelay/semantic_coverage_test.go`
  drives the real `resolveProto()` codecs offline with packets built from real registry IDs. Result:
  **10 stack-carrying families (8 H2C + 2 C2H)** are remapped by `1001`/`2168`/`2169` in both directions, the
  **`ItemRegistry`** table emits `1933`/`1976`/`1976` entries, `plain New()` leaves all 10
  untouched, and the `2193` identity codec keeps the native `882` apple ID. `CraftingData` keeps
  **8 of 11** recipe slices (the two chemistry slices and `MaterialReducers` are dropped);
  `ItemStackRequest`/`ItemStackResponse` carry **0** item runtime IDs. Narrow packed-ID cases
  (`LevelEvent` legacy item particle, `ActorEvent` feed) are mapped `882 → 878`. Biome schemas,
  generic entity metadata, spawn-particle payloads, block-entity NBT and actor identifiers are
  forwarded unchanged with **0** item IDs. See "Semantic packet coverage".
- **NetherNet ingress (`CONN-002`, measured 2026-11-05):** `-listen-nethernet` switches the
  client-facing listener from RakNet to NetherNet/WebRTC. `cmd/xvrelay/nethernet_ingress_test.go`
  runs a **full loopback** — two real `lansignal.Signaling` instances, real LAN discovery, real
  ICE/DTLS/SCTP, real `CONNECTREQUEST` → `CONNECTRESPONSE`, Minecraft Login, `StartGame`, and a
  `Text` packet that crosses the WebRTC data channels — in ~2 s, 3/3 consecutive runs.
  `internal/lansignal/pong_test.go` pins that a `CONNECTREQUEST` sender is routable for the answer
  and that gophertunnel's MCPE pong string is converted to binary `ServerData` (the LAN world card
  decoded as `Minecraft Server` / `Gophertunnel` in the loopback log). See "NetherNet ingress".
- **Live two-machine LAN join (measured 2026-11-05):** a real LAN world (`My World` /
  `Datkiri001`, `192.168.1.119:7551`, protocol `2193`, `network_id=15619059520673756992`) ran on
  a **different machine**. The relay ran with `-listen-nethernet` and advertised its own world
  card on UDP 7551; `cmd/selfcheck -lan-join` discovered that card and joined over
  NetherNet/WebRTC, exactly like a game client picking a LAN world. **13/13 sessions** connected,
  dialled the world and spawned (`client connected` = `dial world OK` = `client has spawned` =
  13). Native `2193`: `1251` packets / `921` chunks / `2076` items / `registry=true` /
  `soak_complete` / `OK=true`. `-reconnect 3` → **4/4** `OK=true`. Sequential
  `1001`/`2168`/`2169` → **3/3** `OK=true` (`1201/941/953` packets, `1933/1976/1976` items).
  Relay log after the two fixes below: **`level=ERROR` 0, `dropped 1 packet` 0,
  `non-offer signal` 0**, and `relay write end (destination closed)` 5 times instead of the
  previous WARN storm. See "Live LAN join and the two NetherNet fixes".
- **Live GUI-client interaction translation (measured 2026-11-05):** a real Minecraft GUI client
  (`ILwHee`, protocol `1001`, `1.26.32`) joined a `2193` LAN world through the relay on
  `0.0.0.0:19164` and stayed 6 minutes. Its relay log exposed two silent C→H defects (hashed
  `BlockRuntimeID` transactions dropped at read time, and 130-vs-131 `ActorFlags` bitsets rejected
  at host write). Both are fixed at the root cause and covered by offline tests plus
  `selfcheck -interactions`. Re-verified with the rebuilt binary: **8/8 sessions** (all four
  protocols + `-reconnect 3`), `level=ERROR` **0**, `dropped 1 packet` **0**,
  `bitset size mismatch` **0**, `client codec returned no packets` **0**, C→H tally `skipped=0`
  with `InventoryTransaction=2` and `ClientMovementPredictionSync=3` per session. See
  "GUI interaction translation (C2H)".
- **Live:** a real 1.26.51 client spawned and rendered terrain (grass, TNT, chest, player model)
  through a 1.26.33 host. That is one path, not all 16.
- **Not verified:** real Minecraft sessions for every pair, add-ons, required/encrypted resource
  packs, long play sessions, and anything the matrix does not name. Semantic payloads are now
  **measured** rather than lumped together: biome schemas, generic entity metadata,
  `SpawnParticleEffect` payloads, block-entity NBT and actor identifiers are forwarded unchanged;
  only the narrow packed-ID cases named in "Semantic packet coverage" are rewritten. The tests
  prove the invariants they check; they do not promise full gameplay compatibility.

## Verification ledger (2026-11-05)

Each change below was pushed as its own commit. Every listed CI run is `completed/success` on all
four jobs (`test (windows)` plus `build` on ubuntu/macos/windows). This table is the traceability
index; the detailed numbers and boundaries live in the linked sections above.

| Task | Change | Commit / CI | Key measurement |
| --- | --- | --- | --- |
| #19 | Live wire item census + host-1001 `CreativeContent` fix | `f903490` / `41968566411` | BDS 1.26.33.1, 4/4 protocols: `5122/5123/5123/5621` stacks at commit time (re-measured after #26: `5118/5121/5120/5620`), `lost=0 unknown=0 ok=true` on all four; found and fixed 5 unmapped host-1001 creative IDs |
| #20 | Hot-path benchmarks (`RELAY-001`) | `b9738da` / `41970545529` | `ns/op`, `B/op`, `allocs/op` for codec / item / chunk paths; `TestBenchFixtures` makes the fixtures fail CI if they ever stop parsing |
| #21 | Concurrency / reconnect / soak (`CONN-004`) | `5e445b7` / `42014656107` | 8-session loopback isolation, `-reconnect 3` 4/4, 3 concurrent clients 3/3, 25 s soak, `-race` clean on the offline harness and the live relay |
| #22 | Resource-pack prefetch + per-dial cost guard (`CONN-005`) | `7ba352d` / `42018501074` | host size lie `594 B declared` vs `734 B real`; prefetch 1 pack in `7.97 s`; budget abort in `35 ms` vs `report` with `overruns=1`; default skip downloads 0 |
| #23 | Semantic packet coverage audit (`PROTO-005`) | `400932c` / `42022254603` | **10** item-stack families + `ItemRegistry` remapped; `CraftingData` keeps **8 of 11** recipe slices; 5 payload families forwarded unchanged; `ItemStackRequest/Response` carry **0** runtime IDs |
| #24 | NetherNet / WebRTC LAN ingress (`CONN-002`) | `b752d5d` / `42028122424` | full loopback: LAN discovery, `CONNECTREQUEST` → `CONNECTRESPONSE`, ICE/DTLS/SCTP, Login, `StartGame`, `Text` round trip; **3/3** runs at ~2 s each |
| #26 | Census extractor covers all 8 H2C families + direction audit of the 10 codec families | `58691e0` / `42030933432` | `stacksFrom()` now handles **8/8 H2C** families; the other 2 are **C2H** and provably cannot reach a client. Re-measured 4 protocols `5118/5121/5120/5620`, `lost=0 unknown=0 ok=true`; `AddPlayer` / `MobArmourEquipment` never occur in the 1-player lab, so the wider extractor does not inflate the count |
| #27 | Live two-machine LAN join with `selfcheck -lan-join` + two NetherNet fixes | `b0a3fe3` / `42037071018` | Real LAN world on another machine (`192.168.1.119:7551`, `My World`, proto `2193`): **13/13** sessions connected / dialled / spawned; `level=ERROR` **0** (was 8 `non-offer signal`), `dropped 1 packet` **0** (was 592), `relay write end (destination closed)` 5. `-reconnect 3` → 4/4 OK; sequential `1001`/`2168`/`2169` → 3/3 OK. Full `go test -race ./...` clean. Measured upstream constraint: the LAN host accepts only one active NetherNet connection at a time |
| #28–#32 | GUI interaction translation (C2H): hashed `UseItem` IDs, no double-map, 130→131 prediction-sync bitset, `selfcheck -interactions` | `c36079c` / `42048437016` | Real GUI client log (`ILwHee`, `1001` → `2193`, 6 min): 13 `bitset size mismatch` drops → **0**. Re-verified **8/8** sessions (four protocols + `-reconnect 3`), `level=ERROR` **0**, `dropped 1 packet` **0**, `client codec returned no packets` **0**, every C→H tally `skipped=0` with `InventoryTransaction=2` + `ClientMovementPredictionSync=3` + 8 other families. Offline **33/33** codec×family no-drop cases + **3/3** hashed `MobEquipment` value checks. Full `go test -race ./...` clean (also fixed a real `*net.UDPAddr` data race in `TestNetherNetIngressLoopback`) |

Only #19, the live parts of #21, #22 and #26 needed the BDS lab; #20, #23, #24 and the #21
loopback harness are fully offline. #27 and #28–#32 were measured against a real LAN world on
another machine (NetherNet and RakNet ingress respectively), not the BDS lab. CI runs `go build`, `go vet` and `go test ./...` (including all new
offline harnesses) but deliberately does **not** run benchmarks or the four opt-in tests gated by
`XVER_MATRIX_REPORT` / `XVER_TERRAIN_CAPTURE` / captured payloads. A full
`CGO_ENABLED=1 go test -race -buildvcs=false ./... -count=1` also passes locally on all six
packages (2026-11-05); race coverage is therefore not limited to the three targeted suites.
`dist/` is ignored, so the repo carries no generated evidence files. The BDS lab is restored to
`transport=nethernet` after every live test.

## Limitations

- **The client must be signed into Xbox Live.** The vendored `2193` fork does not rescue an empty
  `DisplayName`, so an offline client is rejected at login with
  `DisplayName must not be empty`. A real Microsoft account is required on the client side.
- **Real GUI game-client interaction is still user-side verification.** `cmd/selfcheck -interactions`
  is headless: it sends the same 15 C→H packet families a GUI player uses (place/click blocks,
  spawn eggs, hotbar, entity interact) and pins that none is dropped or mis-mapped, but it does not
  render an inventory or move a real avatar. The 6-minute GUI session that exposed these defects
  (`ILwHee`, `1001` → `2193`) is measured; a human re-test of every interaction in the real client
  is the remaining manual step. See "GUI interaction translation (C2H)".
- **Windows launch trap (measured, not a code bug):** `Start-Process -FilePath dist/xvrelay`
  resolves to `dist/xvrelay.exe`. If an older `dist/xvrelay.exe` is left beside a freshly built
  extensionless `dist/xvrelay`, the relay silently runs the stale binary and reproduces old bugs
  (13 `bitset size mismatch` drops, missing `InventoryTransaction`). Build/overwrite **both**
  names from the same source, or always pass the explicit `.exe` path, and verify with
  `sha256sum` (both were `6194977…` after the fix).
- **One active upstream connection per LAN world (measured, upstream constraint).** With
  `-listen-nethernet` the relay dials one NetherNet connection into the LAN world **per client
  session**. A real LAN world hosted by a game client accepts only one active connection at a
  time: 2 concurrent `selfcheck -lan-join` clients → 1 succeeded / 1 disconnected after the host
  closed SCTP; 3 concurrent clients → 2 dropped the same way. All sequential sessions pass
  (13/13 connected, dialled and spawned). Single-client use — the intended manual-test scenario —
  is stable. See "Live LAN join and the two NetherNet fixes".
- **Same-wire sessions were broken until 2026-11-05 — now fixed.** A `1001` client into a `1001`
  BDS host used to log `dial world OK hostProto=1001 clientProto=1001` and then go silent for ~41 s
  before disconnecting itself (`StartGame to the client failed ... context canceled`,
  `clientID113=false`). Root cause: `GameData.Items` was only replaced when the client and host
  protocols **differed**, so a same-wire `1001` client inherited the host's 1933-entry table — but
  the `1001` codec is registry-aware and `ConvertFromLatest` **silently drops** any `ItemRegistry`
  that is not the native 2193 table. The client received no item table and never sent
  `RequestChunkRadius` / `SetLocalPlayerAsInitialised`. `-verify` was ruled out (the failure also
  occurred without it), and a direct BDS dial spawned fine, so the wire was never the problem.
  The fix runs `gameDataItemsFor()` for **every** session; `TestSameWireGameDataItemsNeverDropped`
  pins the invariant. Live re-test with the fixed binary: `ItemRegistry replaced ...
  items=2076 hostItems=1933` → `client sent id=69` → `★ client has spawned into the world`,
  921 LevelChunks, `registry=true`, exit 0.
- **Terrain, block updates, clicked-block IDs, the advertised item registry and item IDs inside
  gameplay packets** are translated for all four client protocols (`1001`/`2168`/`2169`/`2193`).
  Measured by `semantic_coverage_test.go`: item IDs are remapped in **10 stack-carrying packet
  families** plus the `ItemRegistry` table; recipes are **partially** remapped — `CraftingData`
  keeps 8 of 11 slices, dropping the two Education-Edition chemistry slices and `MaterialReducers`
  (the `2193` identity path keeps all 11). Biome definitions, generic entity metadata,
  `SpawnParticleEffect`, block-entity NBT, `AvailableActorIdentifiers` and arbitrary add-on
  payload semantics are **not** re-mapped: the self-check counts those families but does not
  translate their contents. Exceptions are narrow and measured: a packed item ID in `LevelEvent`'s
  legacy particle event and `ActorEvent`'s feed event, plus block runtime IDs in `LevelEvent`
  destroy/crack particles, `LevelSoundEvent`, `AddActor` falling-block metadata and the
  `UpdateBlock*` family. See "Semantic packet coverage".
- **Gameplay item IDs are live-verified at the wire level, not visually.** `2168`/`2169` previously
  used plain `v1_26_45.New()` codecs with no item mapper, so an `InventoryContent` packet kept the
  host's IDs. They now default to `NewWithRegistries` fed the native `2193` table; a plain codec
  passes a 2076-entry `ItemRegistry` through unchanged while the registry-aware one emits 1976. The
  mapping is asserted offline in `item_gameplay_test.go` (both directions, a six-packet smoke
  subset, no drops, non-native input dropped); `-map-gameplay-items=false` bisects it.
  `cmd/itemaudit` logs **by name** exactly which items become AIR per protocol
  (143 / 100 / 100 / 0).   `cmd/clientclone`
  goes further: it decodes every item stack in the **8 host→client packet families** and resolves
  each ID against that client's own table — all four protocols decoded >5,000 named stacks from a
  live BDS with `lost=0 unknown=0` (see "Live item census" below). The remaining 2 families are
  **client→host** and cannot be seen by a client-side census; they are measured relay-side by
  `semantic_coverage_test.go`. In this one-player lab only 5–6 of the 8 H2C families actually
  occur, so the count is still a lower bound. The honest boundary remains: this is a codec-level
  client that decodes stacks, not a game client rendering an inventory UI.
- **Item runtime IDs are int16 on the wire.** `protocol.ItemInstance` writes `NetworkID` with
  `Int16`: an out-of-range probe `900001` decodes as `-17503` even through the identity `2193`
  codec, while in-range unknown IDs (`20000`) survive unchanged. Custom/add-on item IDs outside
  `[-32768, 32767]` therefore cannot survive the wire at all, independently of the relay.
  `cmd/itemaudit` logs this case separately (`WIRE_INT16_TRUNCATION`) so it is not mistaken for
  a missing item.
- **Listener-side `shieldID` was silently wrong until 2026-11-05 — now fixed.** Protocol adapters
  append an 8-byte `BlockingTick` to every item stack whose `NetworkID == ShieldID()`. A
  listener-side `Conn` never receives an `ItemRegistry` from upstream — the relay raw-forwards it
  as a `packet.Unknown` — so it encoded items with `shieldID=0` while the client decoded with the
  shield ID from the table it received. Native 2193 is `358`, target 1001 is `387`, so seeding from
  `GameData.Items` (the native table) is wrong too. The relay now calls
  `Conn.SetShieldID(clientShieldID(...))` before `StartGame`, and `verifyRoundTrip()` uses the
  same target shield instead of hard-coding `0`. Measured with `cmd/clientclone` on a real BDS:
  before `decode_errors=2` / `packets_total=882` (CreativeContent + CraftingData dropped with
  `EOF`), after `decode_errors=0` / `packets_total=1252`. See the terminal-client section below.
- **Ingress supports RakNet (default) and NetherNet (`-listen-nethernet`).** Without the flag,
  clients reach the relay over RakNet; NetherNet is used for LAN discovery (UDP 7551) and, by
  default, for dialling the host (`DialContextNetwork(... minecraft.NetherNet{...})`). With the
  flag, the relay also **accepts** NetherNet clients: the same `lansignal.Signaling` backs both the
  host dial and the incoming listener. `-host-raknet` replaces the host leg with a plain RakNet
  dial for a BDS configured with `transport=raknet`. This ingress capability does **not** change
  the separate host-side boundary below: a BDS with `transport=nethernet` still does not answer
  our outbound dial, so `-host-raknet` remains the measured workaround.
- **Resource-pack prefetch is opt-in, not disabled.** The old hardcoded `if false` before
  `prefetchHostPacks` is gone. `-pack-prefetch` plus `-pack-mode download|limit` runs one extra host
  dial before the listener so even the first client gets the map's packs. Default is
  `-pack-mode skip` with prefetch off — byte-for-byte the previous behaviour. A per-dial guard
  bounds the cost and aborts on a lying host; see "Resource-pack prefetch". Hosts were observed
  misreporting pack sizes (594 B declared / 734 B real on this lab's minimal pack), which is
  exactly what the guard detects.
- **Custom block states** are only handled when they carry explicit typed state properties with
  hashing enabled; runtime-ID ordering of a custom palette is rejected rather than guessed.
- **Protocol IDs are not a promise.** Two builds can share `2193` and still differ in registry
  contents. Snapshots are pinned per protocol, and previews/future builds are unsupported until
  re-pinned.
- **This is a LAN-world relay.** Realms, cloud sessions, NAT traversal and matchmaking are out of scope.
- **Partially measured, still open** (2026-11-05):
  - `CONN-002` — **implemented and loopback-measured** (2026-11-05). `-listen-nethernet` accepts
    NetherNet clients via `minecraft.ListenConfig.ListenNetwork(minecraft.NetherNet{...})`; the
    vendored `go-nethernet` *does* ship a full `Listener` + `Signaling` server, contrary to the
    earlier assumption. Measured: real WebRTC/ICE/DTLS/SCTP loopback, real LAN discovery on UDP
    7551, `CONNECTREQUEST` → `CONNECTRESPONSE`, Login, `StartGame`, and a `Text` packet round trip
    (`cmd/xvrelay/nethernet_ingress_test.go`, 3/3 runs). Still **not** verified: a real Minecraft
    game client joining through the LAN list, NetherNet ingress across two physical machines, and
    Xbox Live identity assertions — `AllowAnonymous: true` is set, so offers without an
    `a=identity` assertion are accepted. The older `CONNECTREQUEST` → no `CONNECTRESPONSE` (25 s
    `context deadline exceeded`, `ServerData v5` 47-byte payload / `TransportLayer=2`) measurement
    was a **host-side** dial into a BDS with `transport=nethernet`; that boundary is unchanged.
  - `CONN-004` — short reconnect (`-reconnect 3`, 4/4 OK), 3-client concurrency (3/3 OK),
    `-race` on both the offline harness and the live relay (0 races), and a 25 s soak are now
    **measured** (see "Concurrency / lifecycle"). Still **not** run: a multi-client, hours-long
    soak; `heap_alloc` grows ~0.04 MB/s in steady state with `num_gc` flat, so a small
    retained-heap trend cannot be excluded. Host dials are also serialised by `hostProtoMu`.
  - `CONN-005` — the host-side prefetch and cost guard are **measured** (see "Resource-pack
    prefetch"): one minimal `.mcpack`, real-vs-declared size, budget abort and report, and a
    client that spawned with packs served. Still **not** tested: required / encrypted / addon
    packs, multi-pack stacks, and a real game client rendering the pack's textures. Note that
    `selfcheck`'s `ResourcePackStack/DataInfo/ChunkData` counters read **0/0/0 even with a pack** —
    login-phase pack packets are consumed internally by gophertunnel (`conn.spawn`), so those
    counters cannot observe the client-side pack download at all. That 0/0/0 is a measurement
    blind spot, not evidence that no pack was served.
  - `PROTO-005` — **measured, not open on coverage**: `semantic_coverage_test.go` pins that 10
    stack-carrying families + `ItemRegistry` are remapped, 8/11 `CraftingData` slices survive,
    two packed-ID particle/feed cases are mapped, and five semantic payload families are forwarded
    untouched (see "Semantic packet coverage"). Still **not** solved: biome schema *semantics*,
    general entity metadata / block-entity NBT translation, full particle payload semantics,
    add-on-defined items/behaviours, and a live census that observes all 10 families. The old
    "recipes / biomes / particles are not solved here" claim was too broad; the recipe loss is now
    measured precisely.
  - `RELAY-001` — per-operation `ns/op` / `B/op` / `allocs/op` are now **measured** for the codec,
    item-mapping and chunk-translation hot paths (see "Hot-path benchmarks"). Process-level CPU
    and end-to-end latency percentiles are still **not measured**: that metric is not portable
    across platforms, and a microbenchmark is not a substitute for a soak test.
  - `CONN-003` — the `ServerData` v5/v6/v7 parser is tested against malformed, truncated and
    trailing-byte payloads, but UDP discovery integration and stale-peer eviction are not.
  - The version→protocol table, block palette and item registry invariants, and the 16-pair wire
    matrix are covered offline. Do not read a missing row here as "verified".

## Layout

```
cmd/xvrelay/                 the relay: listener, dialer, spawn gate, translation wiring, bisect flags
cmd/xvrelay/audit.go         re-parse client-bound chunks, check state IDs against the target registry
cmd/xvrelay/host_recipes1001.go  recipe / creative-item / movement-bitset handling for 1001 hosts
cmd/xvrelay/host_creative1001_test.go  pins host-1001 creative IDs -> native 2193 IDs
cmd/xvrelay/item_gameplay_test.go  offline assertions for gameplay item-ID translation
cmd/xvrelay/block_translation.go  H2C palette translation + C2H interaction scrub / no-double-map / prediction-sync bitset resize
cmd/xvrelay/block_translation_test.go  offline C2H regression tests: hashed UseItem IDs, wrapper unwrap, 33 codec x family no-drop cases
cmd/xvrelay/bench_test.go        hot-path benchmarks (codec / item / chunk) + fixture validity test
cmd/xvrelay/concurrency_test.go  8-session loopback concurrency / isolation / leak / race harness
cmd/xvrelay/packs.go        CONN-005 pack policy + per-dial guard + host-Protocol sniff wrapper
cmd/xvrelay/packs_test.go   policy / budget / real-size lie / overrun abort / skipped-pack tests
cmd/xvrelay/semantic_coverage_test.go  PROTO-005 measured item/particle/NBT family coverage (offline)
cmd/xvrelay/nethernet_ingress_test.go  CONN-002 full NetherNet/WebRTC ingress loopback (real ICE + Login + packet)
cmd/xvrelay/relay_deadconn_test.go  connDead() classifies a closed NetherNet/WebRTC destination via its context
cmd/itemaudit/              offline wire-level audit: logs exactly which items become AIR per protocol + JSON report
cmd/selfcheck/              independent headless Bedrock client: spawn, packet counters, assertions, JSON report; -lan-join joins over NetherNet LAN discovery; -interactions sends all 15 C2H gameplay families
cmd/clientclone/            terminal, command-driven Bedrock client (JSON-lines commands, live BDS/relay tests)
cmd/clientclone/items.go    live wire item census: extract all 8 H2C item-stack families, resolve IDs, ITEM_VISIBLE / ITEM_LOST / ITEM_UNKNOWN_ID
internal/blocktranslate/  block + item palette translation, pinned data, hash lookup
internal/lansignal/       NetherNet LAN discovery on UDP 7551 (world list, signalling address); also the Signaling backend for -listen-nethernet
internal/lansignal/pong_test.go  MCPE-pong -> binary ServerData conversion + CONNECTREQUEST/RESPONSE signalling round trip
internal/lansignal/ingress_test.go  Ingress() keeps outbound-dial signals away from the NetherNet listener
internal/mcport/          Minecraft port detection helpers
java/                     JVM port of the translation layer — status and scope in java/README.md
third_party/              pinned forks of go-nethernet and gophertunnel (protocol 2193 support)
docs/                     protocol matrix, block-translation design notes, research notes
```

## Building and testing

Pure Go — no CGO, no C compiler, no external services. `CGO_ENABLED=0` is set by the `Makefile` and
matters on machines where a host gcc would otherwise be pulled in and fail on `sys/mman.h`.

On Windows, keep `dist/xvrelay` and `dist/xvrelay.exe` (and the same pair for `selfcheck`) built
from the **same** source: `Start-Process` / `CreateProcess` append `.exe` to an extensionless path,
so a stale `dist/xvrelay.exe` will be executed instead of a freshly rebuilt `dist/xvrelay`. Verify
with `sha256sum dist/xvrelay dist/xvrelay.exe` before restarting a live relay; see "Limitations".

```bash
make build     # host binary -> dist/xvrelay
make test      # go test ./...
make bench     # hot-path benchmarks -> dist/bench.txt (ns/op, B/op, allocs/op)
make vet
make selfcheck # headless self-check client -> dist/selfcheck
make itemaudit # offline invisible-item audit -> dist/itemaudit
make clientclone # terminal, command-driven headless client -> dist/clientclone
make release   # linux/amd64, linux/arm64, linux/arm, windows/amd64, darwin/amd64, darwin/arm64
```

Without `make`:

```bash
export CGO_ENABLED=0
go build -buildvcs=false -trimpath -o dist/xvrelay ./cmd/xvrelay
go vet ./...
go test ./...
```

The concurrency harness can additionally be run under the race detector. This is the one command
that needs CGO and a C compiler (it is not part of `make test`):

```bash
CGO_ENABLED=1 go test -race -buildvcs=false ./cmd/xvrelay -run TestConcurrent -count=1
```

The NetherNet ingress test opens real loopback UDP/WebRTC sockets but needs no BDS, no game client
and no Xbox service. Set `XVER_NNET_DEBUG=1` for the full ICE/Login trace when diagnosing it:

```bash
CGO_ENABLED=0 go test -buildvcs=false ./cmd/xvrelay -run TestNetherNetIngressLoopback -count=1 -v
CGO_ENABLED=0 go test -buildvcs=false ./internal/lansignal -run 'TestPongData|TestSignalingRoundTrips' -v
```

`-buildvcs=false` matters when this tree sits inside another git work tree.

Stamp a version at build time (otherwise `-version` prints `dev`, which is the honest answer for a
working tree):

```bash
go build -trimpath -ldflags "-s -w -X main.buildVersion=1.0.0" -o dist/xvrelay ./cmd/xvrelay
./dist/xvrelay -version     # xvrelay 1.0.0
```

The compatibility matrix test is opt-in and writes a report:

```bash
make matrix        # or: XVER_MATRIX_REPORT=dist/matrix.json go test ./cmd/xvrelay -run TestProtocolCompatibilityMatrix -v
```

A relative report path is resolved against the module root (not the package directory the Go
toolchain runs the test in) and its parent directory is created if missing, so this works in a fresh
clone where `dist/` does not exist yet. The test **skips** when `XVER_MATRIX_REPORT` is unset, so a
plain `go test ./...` does not run it — silence there is not a pass.

### Hot-path benchmarks (`RELAY-001`)

`cmd/xvrelay/bench_test.go` times the three paths the relay actually spends time on — codec
conversion, item mapping and chunk translation — using the **real pinned registries** and **real
packet shapes**, entirely offline (no BDS, no listener, no network). The codecs are the same
adapters `resolveProto` / `registryAwareCodec` return, driven with a nil `*minecraft.Conn`, which is
exactly the offline path `clientVersionProtocol` takes. `TestBenchFixtures` also runs under a plain
`go test ./...`, so a fixture that stops parsing fails CI instead of silently producing a fast,
meaningless benchmark.

```bash
make bench     # or, without make:
CGO_ENABLED=0 go test -buildvcs=false ./cmd/xvrelay -run '^$' -bench . -benchmem -benchtime=200ms -count=3
```

Measured 2026-11-05: `windows/amd64`, Go 1.26.2, Intel Core i5-12450H, `CGO_ENABLED=0`,
`-benchtime=200ms -count=3`. Values are the **median of the three runs**; raw output is written to
`dist/bench.txt`. `ns/op` is one packet (or one operation), and `B/op` / `allocs/op` come from
`-benchmem`. Fixture sizes: `InventoryContent` = 36 slots, `CreativeContent` = 2076 native entries,
sub-chunk = v8 / 1 layer / 4 bits per block / 2048 packed bytes / 16-entry palette.

| Path / case | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| `InventoryContent` → `ConvertFromLatest` (H→C), `1001` registry | 2521 | 3416 | 6 |
| `InventoryContent` → `ConvertFromLatest`, `2168` registry | 2785 | 3408 | 6 |
| `InventoryContent` → `ConvertFromLatest`, `2169` registry | 2451 | 3392 | 5 |
| `InventoryContent` → `ConvertFromLatest`, `1001` wire-only | 75 | 72 | 4 |
| `InventoryContent` → `ConvertFromLatest`, `2169` wire-only | 54 | 48 | 3 |
| `InventoryContent` → `ConvertFromLatest`, `2193` identity | 15 | 16 | 1 |
| `InventoryContent` → `ConvertToLatest` (C→H), `1001` registry | 2433 | 3360 | 3 |
| `InventoryContent` → `ConvertToLatest`, `2168` registry | 2419 | 3360 | 3 |
| `InventoryContent` → `ConvertToLatest`, `2169` registry | 2422 | 3360 | 3 |
| `InventoryContent` → `ConvertToLatest`, `1001` wire-only | 16 | 16 | 1 |
| `InventoryContent` → `ConvertToLatest`, `2193` identity | 15 | 16 | 1 |
| `CreativeContent` (2076 items) → `ConvertFromLatest`, `1001` registry | 100066 | 205032 | 7 |
| `CreativeContent` → `ConvertFromLatest`, `2168` registry | 103307 | 205024 | 7 |
| `CreativeContent` → `ConvertFromLatest`, `2169` registry | 103246 | 205008 | 6 |
| `CreativeContent` → `ConvertFromLatest`, `2169` wire-only | 55 | 48 | 3 |
| `CreativeContent` → `ConvertFromLatest`, `2193` identity | 15 | 16 | 1 |
| `mapHostCreative1001` (host-1001 ID fix, 1933 entries) | 151279 | 407024 | 22 |
| item index build (`2193`, 2076 entries) | 76913 | 218528 | 20 |
| `blocktranslate.New(1001→2193)` — once per session | 659506 | 609048 | 149 |
| `Mapper.Map` — single block ID lookup | 4.43 | 0 | 0 |
| `LevelChunk` (inline v8 sub-chunk) translate | 1108 | 4864 | 7 |
| `SubChunk` translate (+ v8→v9 upgrade) | 1215 | 5472 | 8 |
| `UpdateBlock` translate | 33.86 | 40 | 2 |

What the numbers mean: registry-aware conversion costs roughly **160–190×** the identity wire
path (2.4–2.8 µs vs 15 ns), but is still well under 3 µs per 36-slot inventory packet. The
expensive part of a spawn is the one-off full `CreativeContent` remap (~100 µs / 205 KB) and the
`host1001Protocol` creative fix (~151 µs / 407 KB); both happen once, not per tick. The block
mapper build (~660 µs / 609 KB) is likewise once per session, which is why `proto1001` /
`proto2168` / `proto2169` cache it behind `sync.Once`; steady-state chunk translation is ~1.1–1.2 µs
per 2 KB sub-chunk.

Honest boundary: these are **microbenchmarks on synthetic-but-parseable payloads**, not process-level
CPU profiling and not end-to-end latency percentiles. `RELAY-001` now has per-operation numbers
instead of "not measured", but the process-level claim remains deliberately out of scope (it is not
portable across platforms) and a soak test is still needed for tail latency.

### Resource-pack prefetch (`CONN-005`)

The old hardcoded `if false` before `prefetchHostPacks` is gone. Prefetch is now **opt-in** and
guarded, because a host that misreports pack sizes can otherwise hang the dial: `resource.Read()`
failing on a weird manifest version returns early without decrementing `packAmount`, so
`expect(IDResourcePackStack)` never completes and `DialContext` waits out `-dial-timeout`.

```bash
# default: no packs downloaded, no prefetch (previous behaviour)
./dist/xvrelay -host-raknet 127.0.0.1:19132 -host-proto 1001 -listen 127.0.0.1:19164

# bounded prefetch: first client also gets the map's pack
./dist/xvrelay -host-raknet 127.0.0.1:19132 -host-proto 1001 \
  -listen 127.0.0.1:19164 -pack-mode limit -pack-max-mb 8 \
  -pack-budget-mb 32 -pack-overrun abort -pack-prefetch
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-pack-mode` | `skip` | `skip` = refuse every pack; `download` = take all; `limit` = only packs whose **declared** size ≤ `-pack-max-mb` |
| `-pack-max-mb` | `8` | per-pack declared-size ceiling in `limit` mode (`0` = no ceiling) |
| `-pack-budget-mb` | `32` | hard ceiling on total accounted pack bytes **per dial** (`0` = unlimited) |
| `-pack-overrun` | `abort` | `abort` = cancel the host dial; `report` = log and keep downloading |
| `-pack-prefetch` | `false` | one extra host dial before the listener (no-op unless mode is `download`/`limit`) |

How the guard works, with the measured seams in the vendored `2193` fork:

1. `packet.go:74` calls `conn.proto.ConvertToLatest(pk, conn)` for every decoded packet,
   **before** `handlePacket()`. `packSniffProtocol` wraps the host codec there, so it sees
   `ResourcePacksInfo` / `ResourcePackDataInfo` already decoded and cross-version converted.
2. `DownloadResourcePack` has no size argument; the advert table populated in step 1 is what
   lets `decide()` apply `-pack-max-mb` and `-pack-budget-mb` from **declared** sizes.
3. `ResourcePackDataInfo` arrives before the first `ResourcePackChunkRequest` is even sent
   (`conn.go:1313` starts that goroutine after `handleResourcePackDataInfo` returns). It carries
   the **real** compressed size. The guard reconciles the accounted cost, logs a lie, and — if a
   ceiling is now broken — cancels the dial context. `DialContext`'s select has
   `case <-ctx.Done()`, so the wait on spawn ends immediately instead of hanging.
4. DataInfo for a pack that was **not** approved is ignored; those packs consume no bytes.

Live BDS 1.26.33.1 measurement (2026-11-05, `transport=raknet`, one minimal `.mcpack` with UUID
`9f1b0c2e-…`, 674-byte archive):

| Case | Result |
| --- | --- |
| `-pack-mode limit -pack-prefetch` | `advertised=1 (594 B) downloaded=1 skipped=0 size_lies=1 real=734 B accounted=734 B`; prefetch stored **1 pack** in **7.97 s**; relay logged `serving the host's packs to the client count=1`; `selfcheck` `OK=true` (1161 packets, 921 chunks, `registry=true`) |
| `-pack-budget-mb 0.0006 -pack-overrun abort` (629 B, between 594 and 734) | `real accounted total 734 B > -pack-budget-mb 629 B` → dial cancelled in **35 ms**, exit **1**, no hang |
| same budget, `-pack-overrun report` | `overruns=1 policy=report size_lies=1`, `PROBE SUCCEEDED`, exit **0** |
| default `-pack-mode skip` | `downloaded=0 skipped=1 accounted=0 B`, `PROBE SUCCEEDED`, exit **0** |

Honest boundaries: (1) `selfcheck`'s `ResourcePackStack/DataInfo/ChunkData` counters stay
**0/0/0 even with a pack**, because login-phase pack packets are consumed internally by
gophertunnel's `conn.spawn` and never surface on `ReadPacket()` — they cannot prove or disprove
the client-side download. The client-side evidence here is the relay's `FetchResourcePacks` log
plus a successful spawn, not a rendered texture. (2) Only one small, unencrypted, non-required
pack was tested; required/encrypted/addon packs, multi-pack stacks and cache-only packs are not.
(3) `-pack-overrun abort` cancels the dial context; gophertunnel's `DialContext` does not close
the underlying connection on that path, so a host-side goroutine may linger until the transport
disconnects — the same behaviour as the pre-existing `-dial-timeout` path.

### NetherNet ingress (`CONN-002`)

The old note said "NetherNet ingress is not implemented". That confused **two different
directions**. The unimplemented/absent part was always the **host-side** leg: dialling a BDS with
`transport=nethernet` sends `CONNECTREQUEST` and never gets `CONNECTRESPONSE` (25 s timeout). The
**client-facing** leg is a separate question, and the answer is that the vendored libraries already
provide everything needed:

| Piece | Where | What it does |
| --- | --- | --- |
| `nethernet.Signaling` | `third_party/go-nethernet/signal.go` | `Signal` / `Notify` / `Credentials` / `NetworkID` / `PongData` / `Context` |
| `nethernet.ListenConfig.Listen(signaling)` | `third_party/go-nethernet/listener.go` | receives `CONNECTREQUEST` offers, emits `CONNECTRESPONSE`, runs ICE/DTLS/SCTP, `Accept()` returns `net.Conn` |
| `discovery.Listener` | `third_party/go-nethernet/discovery/listener.go` | LAN signalling over UDP 7551 |
| `minecraft.NetherNet.Listen` | `third_party/gophertunnel-2193/minecraft/nethernet.go` | adapts the above to `minecraft.Network` |
| `minecraft.ListenConfig.ListenNetwork` | `third_party/gophertunnel-2193/minecraft/listener.go` | wraps it in the standard `Listener` whose `Accept()` yields `*minecraft.Conn` |

`internal/lansignal.Signaling` already implements `nethernet.Signaling` (it was written to
*discover* LAN worlds and *dial* the host). A single instance can be both halves of a connection:
its `NetworkID()` identifies the relay to clients, `Signal()` routes answers back by peer network
ID, and `Notify()` fans incoming signals out to the inbound listener. So `-listen-nethernet`
reuses the existing `ls` instead of opening a second signalling stack:

```go
listener, err = listenCfg.ListenNetwork(minecraft.NetherNet{
    Signaling: ls.Ingress(), // filtered view: see "Live LAN join and the two NetherNet fixes"
    ListenConfig: nethernet.ListenConfig{AllowAnonymous: true, Log: nnLog},
    Log: nnLog,
}, ls.NetworkID())
```

Everything downstream of `Accept()` is unchanged — it still receives `*minecraft.Conn`, so the
spawn gate, codec wiring and host dial are transport-agnostic. `AllowAnonymous: true` is required
because LAN / self-signed clients send no `a=identity` assertion in their offer; the Minecraft
layer already has `AuthenticationDisabled: true`.

Two supporting fixes were needed in `internal/lansignal`:

| Fix | Why |
| --- | --- |
| `MessagePacket` now calls `record(senderID, addr, nil)` before `deliver()` | `Signal()` resolves a peer by network ID. A client that only ever sent `CONNECTREQUEST` (never a LAN `RequestPacket`) was previously unroutable → `no address known for network ID`. |
| `PongData()` converts a `MCPE;...;` pong string to binary `ServerData` | gophertunnel's listener calls `PongData` every 4 s with a RakNet pong **string**, but a LAN `ResponsePacket` carries **binary** `ServerData`. Without conversion the world card is undecodable. Binary input is still passed through unchanged. |
| `Listen()` allocates network IDs through a process-wide monotonic `atomic.Uint64` seed | `time.Now().UnixNano()` is coarse on Windows: two rapid `Listen()` calls (relay + loopback peer) could receive the **same** network ID, after which each peer discarded the other's packets as "our own broadcast" and `Signal()` could never route the answer back. `pong_test.go` pins 8 rapid instances to distinct IDs. |
| `Ingress()` gives the listener a filtered `Signaling` view; `Signal()` records every OFFER target as a dial target | One `ls` backs **both** the outbound host dial and the inbound listener. `deliver()` broadcasts every received signal to every notifier, so the host's `CONNECTRESPONSE` reached the listener and `go-nethernet` logged `received non-offer signal for a non-existing negotiation` at ERROR level on every join. `dialerNotifier` already filters by connection ID and stays quiet; only the listener needed routing. `internal/lansignal/ingress_test.go` pins that host answers/candidates reach the dialer but not the listener, while client offers still reach the listener. |

Measured 2026-11-05 with `cmd/xvrelay/nethernet_ingress_test.go` (two `lansignal.Signaling`
instances on loopback, real WebRTC, no BDS / game client / Xbox service):

| Step | Result |
| --- | --- |
| LAN discovery | bidirectional in ~2 s (each side records the other's UDP address) |
| `CONNECTREQUEST` → `CONNECTRESPONSE` | delivered through `lansignal.Signal`; sender recorded from the `MessagePacket` alone |
| ICE / DTLS / SCTP | `udp4 host` candidate pair selected, `starting SCTP transport` on both sides |
| Minecraft Login + `StartGame` | `Accept()` yields `*minecraft.Conn`, `RemoteAddr().Network() == "nethernet"` |
| Packet round trip | a `packet.Text` written on the server conn is read back on the client conn |
| Stability | `-count=3` → 3/3 PASS, ~2 s per run |

The loopback logs also confirm the pong fix: the client decodes the relay's `ResponsePacket` as
`Minecraft Server` / `Gophertunnel`. `XVER_NNET_DEBUG=1` prints the full ICE/Login trace.

One gophertunnel quirk is worth recording: `Dialer.DialContextNetwork(ctx, network, address)`
reuses **one** string for both `Network.DialContext` (the NetherNet network ID) and
`ClientData.ServerAddress`, which the listener validates as a resolvable UDP address. A decimal
network ID fails that validation (`ServerAddress must be resolveable as a UDP address`). Real
Bedrock clients send a real `IP:port` in `ServerAddress` and dial the network ID out of band, so
only the loopback dialer needs the `fixedIDNetherNet` shim in the test.

Honest boundaries: (1) the loopback above is two synthetic signalling peers; the **real game
client (GUI)** joining from the LAN world list is still not verified — the live test below uses
the headless `cmd/selfcheck`. (2) `AllowAnonymous: true` means no `Xbox Live identity assertion`
is enforced at the NetherNet layer; the Minecraft layer is `AuthenticationDisabled: true` too.
(3) The **host-side** boundary is unchanged: a BDS with `transport=nethernet` still does not
answer our outbound dial, which is why `-host-raknet` remains the measured workaround.
(4) With `-listen-nethernet` combined with `-host-raknet`, the discovery socket binds UDP 7551
and broadcasts `RequestPacket`s (it is muted/ephemeral in plain direct-host mode), so the relay
is visible on the LAN.

#### Live LAN join and the two NetherNet fixes

Measured 2026-11-05 against a **real LAN world on another machine** (`My World` / `Datkiri001`,
`192.168.1.119:7551`, protocol `2193`, `network_id=15619059520673756992`). Relay:
`./dist/xvrelay -timeout 25s -trace 0 -listen-nethernet`; client:
`./dist/selfcheck -lan-join -proto 2193 -soak 5s`. The relay advertised its own world card
(`network_id=1793885200784033600`) and `selfcheck` discovered it on UDP 7551, then dialled it
over WebRTC instead of RakNet — the same path a game client takes when it picks a LAN world.
`isLocalDiscoveryAddr` accepts the relay's `ResponsePacket` from `192.168.1.219:7551` even when
`127.0.0.1:7551` was probed, while still ignoring the other machine's world card.

Two real defects surfaced only under this live path and were fixed at the root cause:

| Symptom (before) | Root cause | Fix |
| --- | --- | --- |
| `level=ERROR received non-offer signal for a non-existing negotiation` with a `CONNECTRESPONSE` from the host's network ID, once per join | One `lansignal.Signaling` backs both the outbound host dial and the inbound listener; `deliver()` fanned the host's answer out to the listener's notifier, which has no negotiation for that connection ID | `ls.Ingress()` + `dialTargets`: `Signal()` records every OFFER target, and the listener's notifier silently claims signals from those peers. `internal/lansignal/ingress_test.go` |
| `WARN dropped 1 packet, session kept ... nethernet: SCTP transport closed` repeated **592 times** in one relay log after clients disconnected | `connDead()` used only `errors.Is`; go-nethernet reports a closed SCTP transport as a plain `errors.New("nethernet: SCTP transport closed")` that wraps no sentinel, so the H→C loop kept writing into a dead client | `connDead(err, dst)` also checks `dst.Err() != nil`. `minecraft.Conn.ctx` is derived from `nethernet.Conn.Context()` and is canceled on close, so this works for RakNet and NetherNet alike. `cmd/xvrelay/relay_deadconn_test.go` |

After the fixes: **13/13** sessions connected / dialled / spawned, `level=ERROR` **0**,
`dropped 1 packet` **0**, `non-offer signal` **0**, and `relay write end (destination closed)` 5
times (clean termination, no WARN). `-reconnect 3` → 4/4 `OK=true`; sequential
`1001`/`2168`/`2169` → 3/3 `OK=true`. Only two benign WARNs remain: the one-time TOFU private
key generation and `no host packs available to serve the client`.

**Upstream limitation (measured, not a relay bug):** the real LAN world accepts only **one
active NetherNet connection at a time**. With 2 concurrent `selfcheck -lan-join` clients, 1
succeeded and 1 was disconnected after the **host** closed its SCTP transport
(`->15619059520673756992 ... nethernet: SCTP transport closed`); with 3 concurrent clients
(1001/2168/2169), 2 were dropped the same way. All **sequential** runs passed. The relay dials
one host connection per client session by design, so a single client — the intended manual-test
scenario — is stable. Supporting several concurrent clients would require multiplexing all
clients over one host connection, which is a larger architectural change.

### GUI interaction translation (C2H)

Measured 2026-11-05 from a **real Minecraft GUI client** log
(`dist/log/xvrelay-2026-11-05_21-00-09.log`, `ILwHee`, protocol `1001` / `1.26.32`, host `2193`,
6 minutes). `level=ERROR` was already 0, but the C→H tally had `skipped=13` and the log showed
13 `dropped 1 packet` WARNs:

```
dir=C->H type=*packet.ClientMovementPredictionSync id=322
error="panic while encoding *packet.ClientMovementPredictionSync: bitset size mismatch: expected 131, got 130"
```

That log also revealed the *invisible* defect: a packet dropped by the adapter at **read** time
never appears in the relay tally at all, so "no ERROR" is not evidence that translations work.

| Symptom ("place block / click block / spawn egg does nothing") | Root cause | Fix |
| --- | --- | --- |
| Every `UseItem` `InventoryTransaction` / `PlayerAuthInput.ItemInteractionData` vanishes | `UseBlockNetworkIDHashes=true` puts a 32-bit **StateHash** in `BlockRuntimeID`. `NewWithRegistries` assumes numeric runtime IDs, calls `blocks.TargetToNative(hash)`, misses, and `convertGameplayToLatest` returns an **empty slice** — gophertunnel silently drops the packet at read | `scrubInteractionBlocks()` zeroes hashed IDs and forces `ClickAir` while the adapter translates **item** IDs, then restores the original hash/`ActionType` verbatim on the converted packet. It runs on `unwrapWirePacket(pk)` because the 1001/2168/2169 wire pools return a `*translatedPacket` wrapper whose data lives in an unexported `inner` field |
| Clicked/placed block turns into `minecraft:info_update` | The registry-aware client codec already mapped the IDs in `ConvertToLatest`; the C→H loop then ran `reverseBlocks.TranslateInteractions()` on those **native** IDs — a double map | `translateC2HInteractions()` skips the reverse mapper when `adapterHasRegistry(from.Proto().ID())`; wire-only codecs still use it |
| Host rejects movement prediction (`expected 131, got 130`) | A `1001` client sends 130 `ActorFlags`; the native `2193` writer demands `protocol.EntityDataFlagCount` = 131. The relay dropped the packet, so the host rolled the player back | `resizeBitset(..., protocol.EntityDataFlagCount)` in `clientVersionProtocol.ConvertToLatest` (zero-pads the new flag); `host1001Protocol.ConvertFromLatest` does the inverse 131→130 for a `1001` host |

`codecDropLogs` now warns (capped at 5) whenever a client codec returns no packets, so a future
silent read-time drop is visible instead of being mistaken for "no errors".

Offline coverage in `cmd/xvrelay/block_translation_test.go`: 7 targeted tests plus
`TestClientCodecDoesNotDropOtherC2HFamilies` — **33/33** codec×family cases across
`1001`/`2168`/`2169` (`ClientMovementPredictionSync`, `InventoryTransaction`,
`PlayerAuthInput.ItemInteractionData`, `MobEquipment`, `ItemStackRequest`, `Interact`,
`PlayerAction`, `Animate`, `SubChunkRequest`, `SetPlayerInventoryOptions`, `LevelEvent`) and
**3/3** checks that a hashed `MobEquipment.NewItem.Stack.BlockRuntimeID` survives verbatim.

Live harness: `cmd/selfcheck -interactions` sends 15 C→H packets per session (3 prediction sync,
2 `UseItem` `ClickBlock` transactions with a real stone StateHash, 2
`PlayerAuthInput.ItemInteractionData`, and 8 other families). Against the real LAN world with the
rebuilt relay: **8/8 sessions** `OK=true` (four protocols + `-reconnect 3` → 4/4),
`interactions_sent=true` in every JSON report, `level=ERROR` **0**, `dropped 1 packet` **0**,
`bitset size mismatch` **0**, `client codec returned no packets` **0**, and every C→H tally has
`skipped=0` with `ClientMovementPredictionSync=3 InventoryTransaction=2 PlayerAuthInput=2` plus
the 8 other families. Only benign WARNs remain (`no host packs available to serve the client`,
`dropping host custom blocks for a different-version client`).

**Measurement trap (not a code bug):** on Windows, `Start-Process -FilePath dist/xvrelay`
resolves to `dist/xvrelay.exe`, not the extensionless `dist/xvrelay`. A stale `dist/xvrelay.exe`
from an earlier build produced a log that still showed the old `bitset size mismatch` drops and
missing `InventoryTransaction`, which looked like a failed fix. Keep **both** filenames built from
the same source (or always pass the explicit `.exe` path) before restarting the relay; the two
binaries were verified identical with `sha256sum`.

### Semantic packet coverage (`PROTO-005`)

The old limitation text lumped recipes, biomes, entity metadata, particles and block-entity NBT
together as "separate problems and not solved here". That was too broad.
`cmd/xvrelay/semantic_coverage_test.go` builds packets with **real registry IDs** (looked up by
name, never hard-coded), drives them through the same `resolveProto()` codecs the relay installs,
and reads the resulting `NetworkID` fields back through reflection — necessary because
`go-multiversion` returns packets inside an unexported `translatedPacket` wrapper that cannot be
type-asserted from this package. Entirely offline: no BDS, no listener, no network.

```bash
CGO_ENABLED=0 go test -buildvcs=false ./cmd/xvrelay -run TestSemantic -count=1 -v
```

Measured 2026-11-05. Divergent fixture `minecraft:apple`: native `2193` = **882**, `1001` =
**285**, `2168`/`2169` = **878**. H→C = host→client (`ConvertFromLatest`), C→H = client→host
(`ConvertToLatest`).

| Packet family | Direction | Registry-aware `1001`/`2168`/`2169` | `plain New()` | `2193` identity |
| --- | --- | --- | --- | --- |
| `CreativeContent` | H→C | `882 → 285 / 878` | `882` | `882` |
| `CraftingData` (recipe outputs) | H→C | `882 → 285 / 878` | `882` | `882` |
| `AddPlayer` (`HeldItem`) | H→C | `882 → 285 / 878` | `882` | `882` |
| `AddItemActor` | H→C | `882 → 285 / 878` | `882` | `882` |
| `InventoryContent` | H→C | `882 → 285 / 878` | `882` | `882` |
| `InventorySlot` | H→C | `882 → 285 / 878` | `882` | `882` |
| `MobEquipment` | H→C | `882 → 285 / 878` | `882` | `882` |
| `MobArmourEquipment` (5 slots) | H→C | `882 → 285 / 878` (×5) | `882` | `882` |
| `InventoryTransaction` (actions) | C→H | `285 / 878 → 882` | `285 / 878` | `882` |
| `PlayerAuthInput` (`ItemInteractionData`) | C→H | `285 / 878 → 882` | `285 / 878` | `882` |
| `ItemRegistry` (special case before the switch) | H→C | `2076 → 1933 / 1976 / 1976` entries; non-native input dropped | passthrough | `2076` |

So the codec maps **10 stack-carrying families + the registry table**, not the "six" the older
smoke test covered. `plain New()` (the `-map-gameplay-items=false` path) and the native `2193`
identity codec are pinned as pass-through controls, so the mapping is attributable to
`NewWithRegistries`.

Other measured boundaries:

| Case | Measured result |
| --- | --- |
| `CraftingData` recipe slices | **8 of 11** kept on `1001`/`2168`/`2169`; `ShapelessChemistryRecipes`, `ShapedChemistryRecipes` and `MaterialReducers` are set to `nil` by `mapCraftingData`. The `2193` identity path keeps all 11. `host1001Protocol` decodes those slices from the 1001 wire, but the client codec discards them again. |
| `ItemStackRequest` / `ItemStackResponse` | **0** `NetworkID` fields. They use name-based identifiers and dynamic `StackNetworkID`, so there is no item runtime ID to remap; both pass through unchanged. |
| `LevelEvent` legacy item particle (`0x4000\|14`), `ActorEvent` feed | packed item ID in `EventData` mapped `882 → 878`; `plain New()` keeps `882`. |
| `LevelEvent` destroy/crack block, `LevelSoundEvent`, `AddActor` falling-block variant, `UpdateBlock*` | **block** runtime IDs are mapped (source-verified in `gameplay_transactions.go` / `gameplay_mapping.go`); these belong to the block translation path, not the item census. |
| `BiomeDefinitionList`, `SetActorData`, `SpawnParticleEffect`, `BlockActorData` (NBT), `AvailableActorIdentifiers` | forwarded as **1 packet unchanged**, **0** item runtime IDs. `MapBiomeRuntimeID` is defined on every adapter but is **not part of** the vendored `minecraft.Protocol` interface and has **no call site** in this repo or the pinned `go-multiversion` module — it is a Dragonfly chunk-cache hook, not a relay path. |

Honest boundaries: (1) `cmd/clientclone`'s live wire census now extracts all **8 H2C** families,
but the 2 **C2H** families (`InventoryTransaction`, `PlayerAuthInput.ItemInteractionData`) cannot
reach a client at all — they need a relay/server-side hook. Measured on the one-player BDS lab,
only 5–6 of the 8 H2C families actually occur (`AddPlayer`, `MobArmourEquipment` never appear),
so its >5,000 stacks remain a lower bound. Coverage of all 10 families is therefore pinned
**offline** by `semantic_coverage_test.go`, not claimed from the live census.
(2) A "forwarded unchanged" packet is not a claim that its *semantics* are version-compatible:
biome definitions, entity metadata, NBT and particle expressions may still mean different things
across versions, and add-on-defined content is out of scope. (3) These are offline codec
measurements; they do not replace a live cross-version play session.

### Terminal client clone (headless, command-driven)

`cmd/clientclone` is the custom terminal Bedrock client for testing a relay (or a BDS directly)
without a real game client and without a GUI. It is **not** a copy of Cinnabar: that project is
licensed PolyForm Noncommercial 1.0.0, its desktop app is pinned to protocol `2225`, and
`make play` opens a GUI launcher. Only measured client behaviour was reused (a deterministic
`login.ClientData` with skin, 20 Hz `PlayerAuthInput`, latency echo, resource-pack handshake,
chunk-radius / subchunk requests, form responses, `SetLocalPlayerAsInitialised` via
`DoSpawnContext`). The client speaks to the relay's RakNet listener (or to a BDS with `-direct`)
using this repository's own `1001`/`2168`/`2169`/`2193` codecs.

One JSON command per line, from `-script` or stdin. Every command answers with one JSON line;
`-json` writes a final report whose assertions fail a broken session.

```bash
make clientclone
./dist/xvrelay -host-raknet 127.0.0.1:19132 -host-proto 1001 \
  -listen 0.0.0.0:19164 -client-proto auto -verify
./dist/clientclone -script cmd/clientclone/scripts/bds-smoke.jsonl \
  -duration 12s -json dist/clientclone.json
# or drive the four-protocol matrix without editing the script:
./dist/clientclone -auto -addr 127.0.0.1:19164 -proto 2193 \
  -script cmd/clientclone/scripts/bds-smoke.jsonl -duration 12s
```

Commands: `join`, `wait`, `move`, `look`, `jump`, `sneak`, `sprint`, `chat`, `hotbar`, `form`,
`assert`, `dump`, `quit`. `assert` supports `>`, `>=`, `<`, `<=`, `==`, `!=` and `contains`
against any metric listed by `dump`. `-auto` joins at startup from `-addr` / `-direct` / `-proto`.
Bisect flags: `-no-input`, `-no-subchunk`, `-input-flags`, `-nofail`, `-v`. `-item-log N` caps the
`ITEM_*` stderr lines (default 24, `0` = off, `<0` = all).

**Bug this client found and verified (2026-11-05).** On a real BDS 1.26.33.1
(`transport=raknet`) with a `1001` client, the first live run reported `decode_errors=2`,
`bad_packets=2`, `command_failures=1`, `ok=false` and
`read packet: decode packet *v1_26_30.translatedPacket (ID=145): EOF`. The dumped raw payload
decoded cleanly with `shieldID=0` but failed with `387` (15158 bytes left over). Root cause:
protocol adapters append an 8-byte `BlockingTick` to every item stack whose `NetworkID ==
ShieldID()`, and the listener-side `Conn` never receives an `ItemRegistry` from upstream — the
relay raw-forwards it as a `packet.Unknown` — so its `shieldID` stayed `0`. Native 2193 shield is
`358`, target 1001 shield is `387`, so seeding from `GameData.Items` (native) is also wrong. The
relay now calls `Conn.SetShieldID(clientShieldID(...))` before `StartGame`; `verifyRoundTrip()`
uses the same target shield instead of hard-coding `0`; `item_registry_test.go` pins both IDs,
the `+8` byte wire delta and the oracle (an `InventoryContent` shield stack encodes to 38 bytes
with shield `387` and 30 bytes with `0`).

**Live result after the fix** (same BDS, same script): `decode_errors=0`, `bad_packets=0`,
`command_failures=0`, `spawned=true`, `packets_total=1252` (was 882), `CreativeContent=2`
(was 1), `CraftingData=1` (was dropped), `AddItemActor=1`, `ok=true`, exit 0. A four-protocol run
— one session at a time, because this BDS accepts only one direct RakNet host dial at a time —
gave `1001` shield `387`, `2168`/`2169`/`2193` shield `358`, all with `spawned=true`,
`decode_errors=0`, `command_failures=0`, `ok=true`. This is a headless codec-level client: it
does **not** render an inventory and does not replace a real game client.

### Invisible-item audit

`cmd/itemaudit` is offline and needs no BDS, listener, account or GUI client. It logs every item
that becomes AIR (`NetworkID=0`) for each client protocol and writes the full list as JSON:

```bash
make itemaudit                                    # build -> dist/itemaudit
go run ./cmd/itemaudit -limit 20                  # first 20 INVISIBLE lines per protocol
go run ./cmd/itemaudit -proto 2169 -json dist/item-audit.json
go run ./cmd/itemaudit -fail-on-loss             # non-zero exit if anything is invisible (opt-in)
```

Output on the pinned data (2026-11-05): `TOTAL lost=343 target_only=0 protocols=4`, i.e.
`1001` = 143, `2168` = 100, `2169` = 100, `2193` = 0, with `wrong_id=0` for every protocol and
`visible + lost = 2076` in each. The `1001` and `2169` adapters' `MappingReport().ItemFallbacks`
match the measured AIR set exactly (`cross_check="OK: 143 ..."` / `"OK: 100 ..."`); `2168` wraps
`2169` without exporting that report, so its wire measurement is the evidence. `cmd/itemaudit/main_test.go`
runs this as a normal `go test ./...` regression, including the protocol-1001 `translatedPacket`
trap: decoding its `InventoryContent` into a plain latest packet leaves 30219 unread bytes, while
decoding through `codec.Packets(false)` leaves 0.

### Live item census (headless client)

`cmd/clientclone/items.go` answers the question the offline audit cannot: which item stacks did a
real client **actually decode from the live wire**. `stacksFrom()` extracts stacks from **all eight
host→client (H2C) families**: `InventoryContent`, `InventorySlot`, `MobEquipment`, `AddPlayer.HeldItem`,
`MobArmourEquipment` (5 slots), `AddItemActor`, `CreativeContent` and `CraftingData`. Each
`NetworkID` is resolved against `blocktranslate.Items(clientProto)` — the client's own table, not
the native 2193 one. Each stack is logged as `ITEM_VISIBLE` (named item), `ITEM_LOST` (a
creative/recipe entry that arrived as AIR, id 0) or `ITEM_UNKNOWN_ID` (non-zero ID absent from the
table); `items_decoded/visible/lost/unknown/empty_slots`, `items_by_packet` and visible-name samples
go into the JSON report and four new assertions. `AddPlayer` and `MobArmourEquipment` are **not**
consumed by gophertunnel's internal `handlePacket()` switch, so they still reach `ReadPacket()`
even though they arrive near the spawn burst.

```bash
./dist/clientclone -auto -addr 127.0.0.1:19164 -proto 2193 \
  -script cmd/clientclone/scripts/bds-items.jsonl -duration 25s \
  -item-log 8 -json dist/cc-items-2193.json
```

Measured against BDS 1.26.33.1 (`transport=raknet`, relay `-host-raknet 127.0.0.1:19132
-host-proto 1001 -client-proto auto`), one session per protocol (2026-11-05):

| Client proto | stacks decoded | visible | lost | unknown | `ok` |
| --- | --- | --- | --- | --- | --- |
| `1001` | 5118 | 5017 | 0 | 0 | true |
| `2168` | 5121 | 5020 | 0 | 0 | true |
| `2169` | 5120 | 5019 | 0 | 0 | true |
| `2193` | 5620 | 5519 | 0 | 0 | true |

Re-measured 2026-11-05 19:46–19:50 after extending `stacksFrom()` from six to all eight H2C
families (the earlier table at commit `f903490` read `5122/5123/5123/5621`). The small deltas are
session-to-session variation, not new items: `items_by_packet` in this one-player BDS lab contains
only `CreativeContent`, `CraftingData`, `InventoryContent`, `InventorySlot`, `MobEquipment` and
(2193 only) `AddItemActor`. **`AddPlayer` and `MobArmourEquipment` never occur in this world**, so
the wider extractor does not inflate the count — this is measured, not assumed.

**Bug this census found and fixed.** The first `2193` run exposed 5 `CreativeContent` stacks with
IDs `257 / 427 / 716 / 717 / 719` — `copper_spear`, `black_dye` and three pottery sherds. Those
names exist in the native 2193 table with different IDs (`854 / 398 / 689 / 690 / 692`), so the
relay was forwarding **host 1001 IDs unchanged**: the pinned adapter's `convertGameplayToLatest`
has no `CreativeContent` case (its `default` returns the packet untouched), while `CraftingData`
was already handled by `host1001Protocol`. `host1001Protocol.ConvertToLatest` now maps the
converted `CreativeContent` by name through the host `ItemRegistry` into native 2193 IDs. The
mapping must run **after** delegating to the pinned adapter: the incoming packet is an unexported
`translatedPacket` wrapper, and only the converted output is a plain `*packet.CreativeContent`.
Before the fix, `1001`/`2168`/`2169` clients silently dropped those entries in
`targetCreativeContent` (`NativeToTarget` fails on a non-native ID): live `CreativeContent` counts
went `1349 → 1376`, `1369 → 1376`, `1369 → 1376`; the 2193 client went from 5 unknown IDs to 0.
Relay log: `host1001 creative items mapped to native IDs items=1875 mapped=1962 dropped=0
unknown=0`. `host_creative1001_test.go` pins the five IDs and the addon-drop case.

Boundary: this is a headless codec-level client decoding item **stacks**; it still does not render
an inventory UI. The codec remaps **10** stack-carrying families, but only **8 are H2C** —
`stacksFrom()` now handles all 8 of them. The other two (`InventoryTransaction`,
`PlayerAuthInput.ItemInteractionData`) are **client→host** and can never arrive on a client's
`ReadPacket()`; observing them requires a relay/server-side hook, not `clientclone`. So
`items_decoded` is still a lower bound on the wire's item IDs — now because the world/script only
emits a subset of the H2C families (2 of the 8 never occur in this lab) and because C2H traffic is
out of scope for a client, **not** because the extractor is missing cases. See
[Semantic packet coverage](#semantic-packet-coverage-proto-005).

Measured baseline (2026-11-05, CGO disabled): `go build -buildvcs=false ./...`,
`go vet ./...`, `go test -buildvcs=false ./...` all pass, including `cmd/selfcheck`,
`cmd/itemaudit`, `cmd/clientclone` and the five `item_gameplay_test.go` assertions; all six release targets cross-compile
(`linux amd64/arm64/arm`, `windows amd64`, `darwin amd64/arm64`);
`go test -race ./cmd/xvrelay ./internal/blocktranslate` passes with CGO enabled. Four tests are
opt-in and skip by default: `TestProtocolCompatibilityMatrix` (`XVER_MATRIX_REPORT`),
`TestCapturedSubChunkPreservesBlockEntities` (`XVER_TERRAIN_CAPTURE`), and the two dump-dependent
tests in `internal/blocktranslate` (they need a captured host payload). A green CI run is therefore
**not** a claim about those four paths. The live `cmd/selfcheck` matrix is a separate process and
is **not** part of `go test ./...`.

### Platform status

| Target | Status |
| --- | --- |
| `windows/amd64` | built and tested (`go build`, `go vet`, `go test` all green) |
| `linux/amd64` | built and tested under Ubuntu 26.04 LTS (WSL 2), Go 1.27.3 — `go build`, `go vet`, `go test` all green; binary is statically linked |
| `linux/arm64`, `linux/arm`, `darwin/amd64`, `darwin/arm64` | cross-compile cleanly (`CGO_ENABLED=0`); **not executed** — no hardware was available |
| Anything with a real GPU / real client | the relay itself is headless; it never renders |

Only the wire and translation layers are exercised by the test suite. See
[What is actually verified](#what-is-actually-verified) for the boundary between "tested" and
"played".

## Java port

`java/` is a JVM reimplementation of the translation layer, built with nothing but a JDK — no
Maven, no Gradle, dependencies vendored in `java/lib` and pinned by SHA-256.

It is **not a working relay yet**, and it says so at the top of
[`java/README.md`](java/README.md). What it does have is the part that had to be exact before
anything else: `StateHash` reproduces `internal/blocktranslate/hash.go` byte for byte, so all
56 503 pinned block states hash identically on both sides, and `BlockMapper` maps them with the
same two-pass rules as the Go relay.

```bash
make java-check     # or: cd java && ./build.sh check
```

```
1001 -> 2193  states=16913  exact=16329  reduced=72    fallback=512   (96.97% exact)
2168 -> 2193  states=17499  exact=16906  reduced=73    fallback=520   (97.03% exact)
2193 -> 1001  states=22091  exact=16329  reduced=3712  fallback=2050  (90.72% exact)
2193 -> 2168  states=22091  exact=16906  reduced=3768  fallback=1417  (93.59% exact)
```

LAN discovery, RakNet, NetherNet transport, the spawn gate and chunk translation are **not
written** on the Java side. Those layers are where the compatibility risk actually lives, and
shipping them untested against a real client would be worse than not shipping them. The Go relay
is the tested implementation.

## Related work

Different scope, similar problem space — worth reading if you are going down this road:
[ViaBedrock](https://github.com/RaphiMC/ViaBedrock) (Java↔Bedrock),
[Geyser](https://github.com/GeyserMC/Geyser) (Java↔Bedrock),
[gophertunnel](https://github.com/Sandertv/gophertunnel) and
[go-nethernet](https://github.com/df-mc/go-nethernet) (the Go Bedrock stack this is built on),
[CloudburstMC/Data](https://github.com/CloudburstMC/Data) (pinned registries).

## License

**MIT** — see [LICENSE](LICENSE). Copyright (c) 2026 **ILwHee**.

MIT is chosen deliberately: you may use, copy, modify, merge, publish, distribute, sublicense and
sell copies of this code, and you may ship modified versions under your own name — you only have to
keep the copyright notice and the permission notice. Fork it, port it, rewrite it, put it inside a
commercial product. No permission needed, no CLA, no strings.

`third_party/` keeps its upstream licenses (both MIT), and the pinned palette data is Apache-2.0;
see `third_party/*/LICENSE` and `internal/blocktranslate/data/LICENSE.Cloudburst`.

Contributions are the point of this repo — see [CONTRIBUTING.md](CONTRIBUTING.md) for what is easy
to pick up, how to run the checks, and the one rule that matters (measure, do not guess).

Not affiliated with Mojang or Microsoft. "Minecraft" is a trademark of Mojang Synergies AB.
