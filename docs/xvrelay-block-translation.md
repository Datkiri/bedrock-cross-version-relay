> **Note:** this document was written while the tool was still called `xver`. The old name is kept
> AS-IS throughout (`cmd/xver`, the `xver-*.exe` binaries, `log/xver-*.log`) so it matches the logs
> that were actually measured. The tool has since been renamed to `xvrelay`; current source lives
> in `cmd/xvrelay`.

# xver — block translation integration

2026-10-26. Build under test: `xver-blocks.exe`. It turns itself on when the host and client
protocols differ; on the same protocol it keeps the old pass-through path. No Go dependency or
version was changed, and no Java or Node sidecar is needed.

## Research outcome and choice

No drop-in library better than what we have for a NetherNet proxy was found. Spectrum needs its own
backend infrastructure; Ouranos does have a palette rewriter but it is Java and still needs a
per-version registry. The genuinely useful new source is CloudburstMC/Data, which has v1001/v2168/
26.50 snapshots: integrate the real palettes instead of using the 2169 snapshot as the 2193 source.

Pinned sources, licenses and SHA-256 are in `internal/blocktranslate/data/manifest.json`. Each hash
is computed as FNV-1a little-endian over the NBT name/states and cross-checked against the
`network_id` of every entry. V2169 uses the 2168 block schema; a test compares every typed `StateKey`
against the 2169 snapshot of the pinned dependency (the 2168 codec is never substituted for 2169).

## Execution path

- `configureBlockTranslation` runs after the host spawns and takes the exact `hostProtocol`,
  `clientProtocol` and `UseBlockNetworkIDHashes`; it builds a per-session mapper.
- `go-multiversion` `mapping.NewBlockMapperWithTargetOrder` uses the 2193 palette as canonical; it
  never infers an RID from the ordinal position in another version's palette. The snapshot's runtime
  order is preserved.
- H→C: rewrites the palette inside `SubChunk` v8/v9 and inline `LevelChunk`; keeps the rest of the
  `LevelChunk` bytes (biome/border/tile tail). Request-mode keeps the payload ahead of the envelope
  closer, using the right codec.
- H→C: `UpdateBlock`, `UpdateBlockSynced`, `UpdateSubChunkBlocks` (both Blocks/Extra) use the same
  table.
- C→H: the block ID that was clicked/broken in `InventoryTransaction.UseItemTransactionData` and
  `PlayerAuthInput.ItemInteractionData` is mapped back to the host's ID.
- Never mutates the source packet and never applies the mapper twice: `resolveProto` currently uses
  the wire-only `New()`, so `runtime == nil` in those adapters.
- Dialer cache is always `false`; cache-backed terrain is refused so an old blob hash cannot point at
  edited content. A failed translation is never dropped and then logged as success: a translation
  error closes the session and is logged.

## States the client does not have

A known vanilla state with no valid mapping on the client → `minecraft:info_update`, with
`placeholderStates` counted in the log (that is the number of potential states in the table, not the
number of blocks actually encountered). We never quietly call a fallback "exact", and never turn it
into air.
A source ID absent from the palette → an explicit error; no guessing an ID and no borrowing another
session's palette.
Explicit custom typed states carried in `StartGame` keep their hash in both directions; they never
override a known vanilla state's mapping. Custom RID order is not inferred when `hashes=false`. NBT
custom definitions that are not flat typed states, or that would need an addon/resource pack moved
across, are not guaranteed.

## Scope and limits

This translates terrain/update/clicked-block IDs. It does **not** promise full compatibility for
items, recipes, entity metadata, block-entity NBT, particles or biomes. Packed indices and non-block
tails are not semantically translated. Resource-pack rendering and new blocks on an old client still
need real testing. The fallback cannot restore every interaction of a new block the old client does
not have.
The 2193 snapshot comes from 26.50; do not assume every preview or future build sharing that
protocol has the same registry.

## Testing

- `TestSnapshotHashesAnd2169SharedSchema`: SHA-256 and the whole 2169/2168 schema.
- `TestPinnedRegistriesAndAllPairs`/`TestEveryIdentityAndVisibleFallback`: all 16 pairs; identity for
  every RID/hash in each registry; unsupported states come out as a visible placeholder.
- V8/V9, singleton/inline tail, cache/unknown/malformed, dynamic updates, clicked-block reverse,
  custom hash both directions; RED before the implementation, then GREEN.
- `TestProtocolCompatibilityMatrix`: 128 checks over 16 pairs, including source codec → mapper →
  target codec with real block runtime IDs; JSON/TXT in
  `log/protocol-matrix-blocks-2026-10-26.*`.
- Build/vet and the `cmd/xver`/`internal/blocktranslate`/`internal/relay` tests pass.
- Live at 21:08:16 tried a separate listener on 19165; no world `Response` arrived within 10 s
  (peers=3), `log/xver-2026-10-26_21-08-16.log`. So real Minecraft was not verified with the new
  binary. The host app was not restarted, the old relay was not stopped, and no world ID was
  hardcoded.

## Running

Stop the old relay first if you want the same port, then run:

```powershell
.\xver-blocks.exe -timeout 20s
```

To run in parallel while testing, pick a different port:

```powershell
.\xver-blocks.exe -listen 127.0.0.1:19165 -timeout 20s
```

## Sources

- https://github.com/CloudburstMC/Data/tree/a8a4341d7763d6eb8547cff3ca46b4153d60163d
- https://github.com/Blackjack200/Ouranos/blob/master/src/main/java/com/github/blackjack200/ouranos/network/convert/PaletteStorage.java
- https://github.com/shawtymarco/go-multiversion/blob/main/mapping/block.go (the shipped code uses the version pinned in `go.mod`)
- https://github.com/df-mc/dragonfly/blob/master/server/world/chunk/decode.go (both v8/v9 carry a storage count)
- https://github.com/Pumpkin-MC/Pumpkin/blob/master/assets/bedrock/README.md (provenance of the 26.50 dump)

## 2026-10-27 — SubChunk2193 / audit regression

- Latest original log: `log/xver-2026-10-26_23-54-10.log`. Host 1001 connected, the native 2193
  client cancelled after terrain delivery. The audit incorrectly treated hash IDs as runtime
  indices.
- An independent wire fixture found `SubChunkResult` written 1..6 instead of the 2193 schema's 0..5.
  The local `protocol/SubChunkEntry.Marshal` now translates on read/write while keeping the shared
  legacy constants. `TestSubChunk2193ResultWire` failed on every enum before the patch and passed
  afterwards. Mojang schema:
  https://mojang.github.io/bedrock-protocol-docs/1.26.51/types/sub-chunk-packet-data/ . Cloudburst's
  ordinal serialization disagrees; live-client resolution remains to be verified.
- `KnownStateID` checks the negotiated hash/runtime representation; the audit skips non-success
  entries and no longer asserts that a missing pinned ID proves a client crash. The test uses four
  hash IDs copied from the original log (RED then GREEN).
- Restored experiments preservation and the default non-empty request-mode `LevelChunk` payload. The
  existing matrix initially failed 12 checks across 9 pairs; after restoration 128 checks over 16
  pairs pass. The explicit `-shellpayload=true` diagnostic remains.
- `go test ./cmd/xver ./internal/blocktranslate ./internal/relay`, `go vet` and the build passed.
  Binary `xver-fix2193.exe`. Reports `log/protocol-matrix-fix2193-2026-10-27.{json,txt}`. This is
  codec proof, not complete gameplay proof.
- Live at 12:22 on Tung13 reached host 1001 but `-verify` incorrectly dropped 198 `LevelChunk`s: it
  encoded the canonical 1001 sentinel with the native `packet.Marshal` before the target converter.
  Added `TestVerifyLegacyRequestForModernClient` (all 4 failed before the fix, passed afterwards),
  verifying with target wire wrappers/packet constructors and non-mutating sentinel normalisation.
  Tests/vet/build/matrix re-run PASS; a new live check is pending.
