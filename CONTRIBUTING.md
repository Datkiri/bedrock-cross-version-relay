# Contributing

This repo exists to be forked, broken, fixed and rewritten. If you can make a 1.26.51 client survive
a 1.26.33 host for longer than ten minutes, that is a better contribution than any amount of prose.

Everything here is MIT (inbound = outbound). No CLA, no sign-off ceremony, no permission needed.

---

## The one rule

**Measure, do not guess.**

Bedrock's wire format is not documented by Mojang and every assumption you make from a packet name
is probably wrong. This project has already paid for that lesson several times:

- `SubChunkLimit` in the `LevelChunk` packet is **not** a count of sub-chunks — measured, it is `0` or `1`.
- The sub-chunk payload version byte after the storage count is **v9** in `2193` and **v8** below it;
  a v8 payload is not accepted by a v9 reader, but the reverse is fine.
- Block palettes must be matched by **typed state hash**, never by ordinal. Ordinals differ between
  protocol versions even when the block list looks identical.
- A client that "spawned" is not a client that works. Spawn can succeed and the client can still die
  three seconds later while rendering.

So: if you change translation behaviour, include the measurement that proves it. A log line, a byte
dump, a test that fails before your patch and passes after. If you cannot measure it, say so in the
PR instead of asserting it works.

## Non-negotiable invariants

These are not style preferences, they are the safety net that keeps a wrong translation from
silently corrupting a world:

1. **Never map an unknown block to air.** A known vanilla state with no valid target mapping becomes
   `minecraft:info_update` and is counted in the log. Air would make the world look correct while
   actually deleting terrain.
2. **Never silently claim an exact match.** If the source state ID is not in the pinned palette,
   that is a hard error, not a nearest-neighbour guess.
3. **Never translate a palette by index.** Only by typed state hash (FNV-1a little-endian over the
   NBT name plus its state key/value set), cross-checked against `network_id`.
4. **Never let a spawn-time packet reach the client before `StartGame`.** The capture/replay gate
   exists because order matters and a reordered chunk burst kills the client.

## Development setup

Go **1.25+**. No CGO, no C compiler, no external services.

```bash
git clone https://github.com/<you>/bedrock-cross-version-relay
cd bedrock-cross-version-relay

export CGO_ENABLED=0            # required; cgo pulls in a host toolchain that is not used
go build -buildvcs=false ./...
go vet ./...
go test ./...
```

`-buildvcs=false` is not optional on a machine where the repo sits inside another git work tree; it
is harmless everywhere else.

Cross-compile:

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -buildvcs=false -trimpath -o dist/xvrelay-linux-amd64   ./cmd/xvrelay
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -buildvcs=false -trimpath -o dist/xvrelay-linux-arm64   ./cmd/xvrelay
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -buildvcs=false -trimpath -o dist/xvrelay-windows-amd64.exe ./cmd/xvrelay
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -buildvcs=false -trimpath -o dist/xvrelay-darwin-arm64  ./cmd/xvrelay
```

### The diagnostic matrix

`go test ./...` runs the fast checks. The full 4×4 protocol matrix is opt-in because it is a
diagnostic report, not a pass/fail gate:

```bash
XVER_MATRIX_REPORT=matrix.json go test ./cmd/xvrelay -run TestProtocolCompatibilityMatrix -v
```

It produces 16 pairs × 7 checks. Read the `findings` array; a pair with findings is a to-do item, not
a crash.

## Layout

| Path | What lives there |
|---|---|
| `cmd/xvrelay/` | the binary: flags, listener, spawn gate, capture/replay, tests |
| `cmd/xvrelay/protocol_matrix_test.go` | the 4×4 diagnostic matrix |
| `internal/blocktranslate/` | palette loading, typed state hashing, SubChunk v8↔v9, chunk tail preservation |
| `internal/blocktranslate/data/` | pinned palettes `1001.nbt`, `2168.nbt`, `2193.nbt` + `manifest.json` (commit + SHA-256) |
| `internal/lansignal/` | LAN discovery on **UDP 7551**, `ServerData` v5/v6/v7 decoding |
| `internal/mcport/` | protocol-ID ↔ version mapping |
| `third_party/gophertunnel-2193` | vendored gophertunnel fork with the `2193` wire codec |
| `third_party/go-nethernet` | vendored NetherNet transport |

`go.mod` uses local `replace` directives, so the repo builds standalone — there is nothing to
`go get` from a private host.

## Good first contributions

Ordered roughly by how much you have to know before you can start.

- **Documentation of what actually breaks.** Reproduce a failure, write down the packet, the byte
  offset, and the client version. That is worth a PR on its own.
- **Packaging.** Linux `.tar.gz` with a systemd unit, a `Dockerfile`, a Homebrew formula, an AUR
  `PKGBUILD`. Nothing in the engine blocks this.
- **CLI ergonomics.** `-world` matching is a substring match on the discovery name; a proper
  interactive picker would help a lot of users.
- **Biome translation.** Currently the biome tail of a chunk is carried through untouched. If the
  biome ID spaces differ between the two versions, this is wrong and nobody has measured it yet.
- **Entity metadata / actor properties.** Not translated at all today. A version-mismatched actor
  flag table will produce invisible or T-posing entities.
- **Resource pack negotiation.** The relay forwards pack offers; it does not reconcile two different
  pack sets.
- **A second language implementation.** A Java port using CloudburstMC/Protocol is the most requested
  kind of contribution, because a lot of server operators already run a JVM. See `java/` if it exists
  yet; if it does not, open an issue before writing 5k lines.

## Adding a new protocol version

This is the highest-value contribution and it has a fixed recipe.

1. **Pin the palette.** Take `blocks.nbt` from [CloudburstMC/Data](https://github.com/CloudburstMC/Data)
   at the commit matching the target version, gzip it, drop it in
   `internal/blocktranslate/data/<protocolID>.nbt`, and add an entry to `manifest.json` with the
   commit hash, the state count, and the SHA-256 of the file you actually wrote.
2. **Add the codec.** In `cmd/xvrelay`, extend `resolveProto` with the new ID and the gophertunnel
   version that implements it. If the version is not in the vendored fork, vendor it under
   `third_party/` rather than bumping the module — the `2193` fork is deliberately pinned.
3. **Check the wire deltas.** Diff the new `LevelChunk`/`SubChunk` layout against the closest
   existing version. If the sub-chunk version byte moved, update the upgrade path in
   `internal/blocktranslate/network.go` and add a regression test next to
   `subchunk2193_regression_test.go`.
4. **Extend the matrix.** Add the ID to the `ids` slice in `protocol_matrix_test.go` and record the
   new pairs in the PR description — including the ones that fail.
5. **Say what you did not verify.** A new version that passes the wire matrix has not been played.
   Write that down.

## Pull requests

- One concern per PR. "Fix sub-chunk v9" and "reformat everything" are two PRs.
- Describe the measurement, not the intention. Before/after, with numbers.
- New behaviour needs a test that fails without the change. `go test ./...` must be green.
- If you are changing translation output, include a short sample of the log so a reviewer can see the
  placeholder count and the mapping direction.
- Bug reports are more useful with: client version, host version, the exact flags used, and the log
  line where it went wrong.

## Licence

By contributing you agree your contribution is licensed under the MIT licence in [LICENSE](LICENSE),
Copyright (c) 2026 ILwHee. You keep your own copyright on what you write; you are just granting
everyone else the same rights you got.
