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
```

Point the older/newer client at `<relay-ip>:19164` using **Add Server**. The relay logs both
directions; `-trace 40` (default) prints the first 40 packet names per direction, `-trace 0` silences it.

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
  (currently `1001`) translates `ItemRegistry` itself and expects the **native `2193`** table as
  input — feeding it the client's own table makes validation fail and the packet is dropped. A
  **registry-less** codec (`2168`/`2169` use plain `New()`) passes it through, so it must be handed
  the client's own table. `clientItemRegistry()` implements that rule; both branches are tested.
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
| `-listen` | `0.0.0.0:19164` | address the clients connect to |
| `-world` | *(auto)* | level name or NetworkID; empty = the only world found |
| `-timeout` | `20s` | LAN discovery window |
| `-client-proto` | `auto` | force the client-side protocol (`1001`/`2168`/`2169`/`2193`) |
| `-host-proto` | `auto` | force the host-side protocol |
| `-trace` | `40` | print the first N packet names per direction, `0` = off |
| `-dial-timeout` | `60s` | max time to dial the host, resource-pack download included |
| `-startgame` | `raw` | `raw` = replay the host's original bytes when the wire matches, `rebuild` = re-encode |
| `-identity` | `auto` | send the client's real name + XUID, or `bot` to present as Steve |
| `-noblocktranslate` | off | disable palette translation (keeps chunk framing) — debugging aid |
| `-verify` | off | encode→decode every client-bound packet and drop the broken ones (slower, diagnostic) |
| `-audit` | off | re-parse client-bound chunks and check state IDs against the target registry |
| `-probe` | off | dial the world once and exit, no client needed |

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
- **Live:** a real 1.26.51 client spawned and rendered terrain (grass, TNT, chest, player model)
  through a 1.26.33 host. That is one path, not all 16.
- **Not verified:** real Minecraft sessions for every pair, add-ons, resource packs, biomes, entity
  metadata, particles, long play sessions, and anything the matrix does not name. The tests prove the
  invariants they check; they do not promise full gameplay compatibility.

## Limitations

- **The client must be signed into Xbox Live.** The vendored `2193` fork does not rescue an empty
  `DisplayName`, so an offline client is rejected at login with
  `DisplayName must not be empty`. A real Microsoft account is required on the client side.
- **Terrain, block updates, clicked-block IDs and the advertised item registry** are translated.
  Recipes, biome schemas, entity metadata, block-entity NBT, particles and arbitrary add-on content
  are separate problems and are **not** solved here.
- **Item IDs inside gameplay packets are only mapped when the host codec is registry-aware.** The
  relay advertises the correct item table to every client, but `2168`/`2169` hosts use the plain
  `v1_26_45.New()` codec (no item mapper), so an `InventoryContent` packet from such a host keeps
  the host's IDs. Measured: `v1_26_45.New().ConvertFromLatest` passes an `ItemRegistry` of 2076
  native entries through unchanged, while `NewWithRegistries` emits 1976. Turning those codecs into
  registry-aware ones would fix this, but it changes many gameplay packets at once and needs a real
  client to validate — it is deliberately left as the next step.
- **Ingress is RakNet only.** Clients reach the relay over RakNet; NetherNet is used for LAN discovery
  (UDP 7551) and for dialling the host (`DialContextNetwork(... minecraft.NetherNet{...})`). A client
  that can only speak NetherNet — for example one joining through an activity handle instead of the
  LAN list — cannot connect. This is a design boundary, not a bug.
- **Resource-pack prefetch is disabled** (`if false` before `prefetchHostPacks`): hosts were observed
  misreporting pack sizes, which made the download hang. Packs are still served to the client from the
  most recent host dial; only the pre-dial is off.
- **Custom block states** are only handled when they carry explicit typed state properties with
  hashing enabled; runtime-ID ordering of a custom palette is rejected rather than guessed.
- **Protocol IDs are not a promise.** Two builds can share `2193` and still differ in registry
  contents. Snapshots are pinned per protocol, and previews/future builds are unsupported until
  re-pinned.
- **This is a LAN-world relay.** Realms, cloud sessions, NAT traversal and matchmaking are out of scope.
- **Not yet covered by tests or a real client** (2026-11-05): multi-transport/NetherNet ingress
  (CONN-002), UDP discovery integration and stale-peer eviction (CONN-003), live multi-client
  soak/reconnect (CONN-004), resource-pack negotiation edge cases (CONN-005), and any CPU/RAM/
  latency benchmark (RELAY-001). What **is** covered offline: the `ServerData` v5/v6/v7 parser
  against malformed, truncated and trailing-byte payloads; the version→protocol table including
  unknown/mismatched hosts; the block palette and item registry invariants; and the 16-pair wire
  matrix. Do not read a missing row here as "verified".

## Layout

```
cmd/xvrelay/                 the relay: listener, dialer, spawn gate, translation wiring, bisect flags
cmd/xvrelay/audit.go         re-parse client-bound chunks, check state IDs against the target registry
cmd/xvrelay/host_recipes1001.go  recipe / movement-bitset handling for 1001 hosts
internal/blocktranslate/  block + item palette translation, pinned data, hash lookup
internal/lansignal/       NetherNet LAN discovery on UDP 7551 (world list, signalling address)
internal/mcport/          Minecraft port detection helpers
java/                     JVM port of the translation layer — status and scope in java/README.md
third_party/              pinned forks of go-nethernet and gophertunnel (protocol 2193 support)
docs/                     protocol matrix, block-translation design notes, research notes
```

## Building and testing

Pure Go — no CGO, no C compiler, no external services. `CGO_ENABLED=0` is set by the `Makefile` and
matters on machines where a host gcc would otherwise be pulled in and fail on `sys/mman.h`.

```bash
make build     # host binary -> dist/xvrelay
make test      # go test ./...
make vet
make release   # linux/amd64, linux/arm64, linux/arm, windows/amd64, darwin/amd64, darwin/arm64
```

Without `make`:

```bash
export CGO_ENABLED=0
go build -buildvcs=false -trimpath -o dist/xvrelay ./cmd/xvrelay
go vet ./...
go test ./...
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

Measured baseline (2026-11-05, CGO disabled): `go build ./...`, `go vet ./...`, `go test ./...`
all pass; all six release targets cross-compile (`linux amd64/arm64/arm`, `windows amd64`,
`darwin amd64/arm64`); `go test -race ./cmd/xvrelay ./internal/blocktranslate` passes with CGO
enabled. Four tests are opt-in and skip by default: `TestProtocolCompatibilityMatrix`
(`XVER_MATRIX_REPORT`), `TestCapturedSubChunkPreservesBlockEntities` (`XVER_TERRAIN_CAPTURE`),
and the two dump-dependent tests in `internal/blocktranslate` (they need a captured host payload).
A green CI run is therefore **not** a claim about those four paths.

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
