> **Note:** this document was written while the tool was still called `xver`. The old name is kept
> AS-IS throughout (`cmd/xver`, the `xver-*.exe` binaries, `log/xver-*.log`) so it matches the logs
> that were actually measured. The tool has since been renamed to `xvrelay`; current source lives
> in `cmd/xvrelay`.

# Block translation research for xver — 2026-10-26

## Conclusion

The lowest-churn direction for this repo: reuse the pinned `go-multiversion`
`mapping.BlockMapper`, paired with the `SubChunk` palette rewriter that already exists in a separate
client project. This is a conclusion drawn from source code — at the time of writing it was not yet
a feature integrated into xver.

## Components examined

1. `go-multiversion/mapping/block.go` (local commit `7d7f3d51d090`): `NewBlockMapper`/
   `NewBlockMapperWithTargetOrder`, `StateKey`, `MapNative`, `TargetToNative`,
   `NativeState`/`TargetState`, `Fallbacks`. It can supply a `BlockRegistry` through a small
   adapter; running a full Dragonfly server is not required. You must feed it the correct version's
   canonical registry and RID order. `MapNative` distinguishes valid from exact; an air fallback is
   never treated as an exact mapping.
2. `protocols/v1_26_30` and `v1_26_45`: `NewWithRegistries`; `convertGameplay` handles
   `UpdateBlock`/`UpdateBlockSynced`/`UpdateSubChunkBlocks`. There is no case rewriting
   `LevelChunk`/`SubChunk` `RawPayload` in the switch that was read. Wiring in `NewWithRegistries` is
   not by itself enough for a proxy that forwards chunk bytes received from the host.
3. The separate client project's `internal/mcstructure/network.go`: `TranslateNetworkSubChunk(data,
   mapID)` rewrites each palette entry and keeps the packed indices; supports v8/v9 under the current
   parser, and errors when an ID does not map or the payload is unsupported.
   `TestTranslateNetworkSubChunk` passed.
4. The same client project's `cmd/bridge/legacy_chunk.go`: `translateLegacySubChunk`,
   `networkBlockHash`, `mapID`, `getLegacyBlockSupport`. Both registry/visual-state normalisation
   tests passed. **WARNING:** `buildLegacyBlockSupport` uses `data2169.BlockStates` as
   `nativeStates`; do not call that a complete 2193 registry. `mapID` currently replaces unknown with
   air, so this policy was deliberately NOT copied into xver as if it were exact.
5. xver's `go.mod` already has `worldupgrader` 1.0.22 indirectly; it upgrades state schemas but does
   not translate network RIDs/chunks on its own, and does not create blocks the client lacks.

## Suggested integration

- One registry table per protocol, keeping the `runtimeID` order distinct from the state hash.
- Match by name + typed properties, normalise through the schema where there is a basis for it; use a
  precomputed lookup for the source/target pair rather than looping over 4096 blocks per packet when
  only the palette changes.
- Map host → canonical → client when several pairs are involved; only use runtimeID tables once the
  registration order is confirmed. With `UseBlockNetworkIDHashes` you must map hash → state → hash,
  never index an RID array directly.
- A single mapper for `SubChunk`/inline `LevelChunk` and the related
  `UpdateBlock`/`UpdateSubChunkBlocks`. Avoid double mapping when the codec already does
  registry-aware conversion.
- Keep biomes, block-entity NBT and blob cache out of the block table: a cached blob hash must be
  recomputed/rewritten, or negotiation must be disabled properly when content changes; never keep an
  old hash for edited data.
- New/custom blocks with no client counterpart need either a transparent replacement policy or a
  compatible resource pack; report exact/fallback/unknown coverage. Do not promise to translate 100%
  of every addon.
- The native 2193 palette is an input that still needs verification; `StartGame.Blocks` usually
  carries custom entries only, not a full replacement vanilla registry. The 2169 snapshot must not be
  substituted for the 2193 RIDs.

## Upstream cross-check

- https://github.com/shawtymarco/go-multiversion/blob/main/mapping/block.go — mapper and fallback.
- https://github.com/shawtymarco/go-multiversion#usage — registry-aware integration; the current
  README is not automatically the API of the pinned version.
- https://github.com/Blackjack200/Ouranos/blob/master/src/main/java/com/github/blackjack200/ouranos/network/convert/PaletteStorage.java — `readNetwork`/`writeNetwork` through callbacks.
- https://github.com/Blackjack200/Ouranos/blob/master/src/main/java/com/github/blackjack200/ouranos/network/convert/BlockStateDictionary.java — per-protocol table, canonical state/hash, `info_update` fallback.
- https://github.com/Blackjack200/Ouranos/blob/master/src/main/java/com/github/blackjack200/ouranos/network/convert/TypeConverter.java — rewrites chunk/subchunk/biome/block entities; useful for the algorithm, but it does not prove xver's specific 1.26 pairs.
- https://github.com/CloudburstMC/Protocol — codec library; changing the codec does not by itself
  provide xver with a block remap table.
- https://github.com/df-mc/worldupgrader — schema upgrades through PMMP generated schemas.
- `EndstoneMC/bedrock-server-data` branch `v2` release 1.26.51 metadata was read: it only confirms
  the BDS link/checksum; no 2193 palette was found there. Do not infer that a palette exists.

## Outcome of this round

Research and notes only — no product code changed, no dependency added, the host app and Minecraft
were not driven. The reused tests listed above all pass; block translation inside xver was not
verified live.
